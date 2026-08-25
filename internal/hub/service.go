package hub

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/notzree/zync/internal/db/dbgen"
	"github.com/notzree/zync/internal/protocol"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// LeaseService implements the coordination logic: workspace enrollment and
// the take/release lease protocol with generation fencing.
type LeaseService struct {
	db   *sql.DB
	q    *dbgen.Queries
	git  *GitManager
	blob *BlobStore
	ttl  time.Duration
	now  func() time.Time
}

func NewLeaseService(conn *sql.DB, git *GitManager, blob *BlobStore, cfg Config) *LeaseService {
	return &LeaseService{db: conn, q: dbgen.New(conn), git: git, blob: blob, ttl: cfg.LeaseTTL, now: time.Now}
}

func newPushToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// EnsureReplica registers/refreshes a replica. Non-empty opencodeURL and
// workspacesDir update the stored advertisement; empty values preserve it.
func (s *LeaseService) EnsureReplica(ctx context.Context, name, opencodeURL, workspacesDir string) error {
	if name == "" {
		return nil
	}
	_, err := s.q.UpsertReplica(ctx, dbgen.UpsertReplicaParams{
		Name:          name,
		OpencodeUrl:   opencodeURL,
		WorkspacesDir: workspacesDir,
	})
	return err
}

func (s *LeaseService) ListReplicas(ctx context.Context) ([]protocol.ReplicaInfo, error) {
	rows, err := s.q.ListReplicas(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.ReplicaInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, protocol.ReplicaInfo{
			Name:          r.Name,
			OpencodeURL:   r.OpencodeUrl,
			WorkspacesDir: r.WorkspacesDir,
			LastSeenAt:    r.LastSeenAt.String,
		})
	}
	return out, nil
}

func (s *LeaseService) CreateWorkspace(ctx context.Context, name, defaultBranch string) (protocol.WorkspaceInfo, error) {
	if !ValidWorkspaceName(name) {
		return protocol.WorkspaceInfo{}, fmt.Errorf("%w: invalid workspace name %q", ErrConflict, name)
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	if _, err := s.q.GetWorkspaceAnyByName(ctx, name); err == nil {
		return protocol.WorkspaceInfo{}, fmt.Errorf("%w: workspace %q already exists", ErrConflict, name)
	}
	ws, err := s.q.CreateWorkspace(ctx, dbgen.CreateWorkspaceParams{Name: name, DefaultBranch: defaultBranch})
	if err != nil {
		return protocol.WorkspaceInfo{}, err
	}
	if err := s.git.EnsureBareRepo(name, defaultBranch); err != nil {
		return protocol.WorkspaceInfo{}, err
	}
	return protocol.WorkspaceInfo{Name: ws.Name, DefaultBranch: ws.DefaultBranch}, nil
}

func (s *LeaseService) GetWorkspace(ctx context.Context, name string) (protocol.WorkspaceInfo, error) {
	ws, err := s.q.GetWorkspaceByName(ctx, name)
	if err != nil {
		return protocol.WorkspaceInfo{}, fmt.Errorf("%w: workspace %q", ErrNotFound, name)
	}
	return s.workspaceInfo(ctx, ws)
}

func (s *LeaseService) workspaceInfo(ctx context.Context, ws dbgen.Workspace) (protocol.WorkspaceInfo, error) {
	rows, err := s.q.ListLeasesByWorkspace(ctx, ws.ID)
	if err != nil {
		return protocol.WorkspaceInfo{}, err
	}
	info := protocol.WorkspaceInfo{Name: ws.Name, DefaultBranch: ws.DefaultBranch, ArchivedAt: ws.ArchivedAt.Int64}
	for _, r := range rows {
		info.Leases = append(info.Leases, leaseInfo(r.WorkspaceName, r.Branch, r.State, r.HolderName, r.HolderOpencodeUrl, r.HolderWorkspacesDir, r.Generation, r.SnapshotCommit, r.BaseCommit, r.ExpiresAt, r.UpdatedAt, s.now().UnixMilli()))
	}
	return info, nil
}

// Take grants the (workspace, branch) lease to replica. Idempotent when the
// replica already holds it. With force, an active lease held by another
// replica is broken: the generation bump invalidates the old holder's push
// token, fencing it out at the git layer.
func (s *LeaseService) Take(ctx context.Context, wsName, branch, replica string, force bool) (protocol.TakeResponse, error) {
	if branch == "" || replica == "" {
		return protocol.TakeResponse{}, fmt.Errorf("%w: branch and replica are required", ErrConflict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.TakeResponse{}, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	ws, err := q.GetWorkspaceByName(ctx, wsName)
	if err != nil {
		return protocol.TakeResponse{}, fmt.Errorf("%w: workspace %q", ErrNotFound, wsName)
	}
	rep, err := q.UpsertReplica(ctx, dbgen.UpsertReplicaParams{Name: replica})
	if err != nil {
		return protocol.TakeResponse{}, err
	}

	now, expiresAt := s.deadline()
	lease, err := q.GetLease(ctx, dbgen.GetLeaseParams{WorkspaceID: ws.ID, Branch: branch})
	if errors.Is(err, sql.ErrNoRows) {
		created, err := q.CreateHeldLease(ctx, dbgen.CreateHeldLeaseParams{
			WorkspaceID:     ws.ID,
			Branch:          branch,
			HolderReplicaID: sql.NullInt64{Int64: rep.ID, Valid: true},
			PushToken:       sql.NullString{String: newPushToken(), Valid: true},
			ExpiresAt:       expiresAt,
		})
		if err != nil {
			return protocol.TakeResponse{}, err
		}
		if err := tx.Commit(); err != nil {
			return protocol.TakeResponse{}, err
		}
		return s.takeResponse(created), nil
	}
	if err != nil {
		return protocol.TakeResponse{}, err
	}

	expired := lease.State == "held" && lease.ExpiresAt.Valid && lease.ExpiresAt.Int64 <= now
	if lease.State == "held" && !expired {
		if lease.HolderReplicaID.Valid && lease.HolderReplicaID.Int64 == rep.ID {
			renewed, err := q.RenewLease(ctx, dbgen.RenewLeaseParams{
				NewExpiresAt:    expiresAt,
				ID:              lease.ID,
				HolderReplicaID: sql.NullInt64{Int64: rep.ID, Valid: true},
				Generation:      lease.Generation,
				PushToken:       lease.PushToken,
				Now:             sql.NullInt64{Int64: now, Valid: true},
			})
			if err != nil {
				return protocol.TakeResponse{}, fmt.Errorf("%w: lease expired while renewing", ErrConflict)
			}
			if err := tx.Commit(); err != nil {
				return protocol.TakeResponse{}, err
			}
			return s.takeResponse(renewed), nil
		}
		if !force {
			holder := "unknown"
			if rows, err := q.ListLeasesByWorkspace(ctx, ws.ID); err == nil {
				for _, r := range rows {
					if r.Branch == branch && r.HolderName.Valid {
						holder = r.HolderName.String
					}
				}
			}
			return protocol.TakeResponse{}, fmt.Errorf("%w: branch %q is held by %q (use --force to break the lease)", ErrConflict, branch, holder)
		}
	}

	granted, err := q.GrantLease(ctx, dbgen.GrantLeaseParams{
		HolderReplicaID: sql.NullInt64{Int64: rep.ID, Valid: true},
		PushToken:       sql.NullString{String: newPushToken(), Valid: true},
		ExpiresAt:       expiresAt,
		ID:              lease.ID,
	})
	if err != nil {
		return protocol.TakeResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.TakeResponse{}, err
	}
	return s.takeResponse(granted), nil
}

// Release records the flush (snapshot + base commits) and frees the lease.
// The generation must match the caller's grant: a stale holder that was
// force-broken cannot release over a newer grant.
func (s *LeaseService) Release(ctx context.Context, wsName string, req protocol.ReleaseRequest, replica string) error {
	// GC must observe either the old database roots or the completed release.
	// The pushed ref can move concurrently, but it protects the new objects
	// while this process-local barrier keeps SQLite retention roots coherent.
	return s.git.withGCBarrier(func() error {
		return s.release(ctx, wsName, req, replica)
	})
}

func (s *LeaseService) release(ctx context.Context, wsName string, req protocol.ReleaseRequest, replica string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	ws, err := q.GetWorkspaceByName(ctx, wsName)
	if err != nil {
		return fmt.Errorf("%w: workspace %q", ErrNotFound, wsName)
	}
	rep, err := q.UpsertReplica(ctx, dbgen.UpsertReplicaParams{Name: replica})
	if err != nil {
		return err
	}
	lease, err := q.GetLease(ctx, dbgen.GetLeaseParams{WorkspaceID: ws.ID, Branch: req.Branch})
	if err != nil {
		return fmt.Errorf("%w: no lease for branch %q", ErrNotFound, req.Branch)
	}
	if lease.State != "held" || !lease.HolderReplicaID.Valid || lease.HolderReplicaID.Int64 != rep.ID {
		return fmt.Errorf("%w: lease not held by %q", ErrConflict, replica)
	}
	if lease.Generation != req.Generation {
		return fmt.Errorf("%w: stale generation %d (current %d)", ErrConflict, req.Generation, lease.Generation)
	}
	var agentDigest, agentFormat, agentSession sql.NullString
	var agentSize, agentGeneration sql.NullInt64
	var extrasDigest, extrasFormat sql.NullString
	var extrasSize, extrasGeneration sql.NullInt64
	if req.AgentState != nil {
		bundle := req.AgentState
		if !validDigest(bundle.Digest) || bundle.Size < 0 || bundle.Size > protocol.MaxAgentStateBytes ||
			bundle.Format != protocol.AgentStateFormat || !validSessionID(bundle.SessionID) || bundle.SourceGeneration != req.Generation {
			return fmt.Errorf("%w: invalid agent-state bundle", ErrConflict)
		}
		if !s.blob.Exists(bundle.Digest, bundle.Size) {
			return fmt.Errorf("%w: agent-state blob was not uploaded", ErrConflict)
		}
		agentDigest = sql.NullString{String: bundle.Digest, Valid: true}
		agentSize = sql.NullInt64{Int64: bundle.Size, Valid: true}
		agentFormat = sql.NullString{String: bundle.Format, Valid: true}
		agentSession = sql.NullString{String: bundle.SessionID, Valid: true}
		agentGeneration = sql.NullInt64{Int64: req.Generation, Valid: true}
	}
	if req.Extras != nil {
		bundle := req.Extras
		if !validDigest(bundle.Digest) || bundle.Size < 0 || bundle.Size > protocol.MaxExtrasBytes ||
			bundle.Format != protocol.ExtrasFormat || bundle.SourceGeneration != req.Generation {
			return fmt.Errorf("%w: invalid encrypted extras bundle", ErrConflict)
		}
		if !s.blob.Exists(bundle.Digest, bundle.Size) {
			return fmt.Errorf("%w: encrypted extras blob was not uploaded", ErrConflict)
		}
		extrasDigest = sql.NullString{String: bundle.Digest, Valid: true}
		extrasSize = sql.NullInt64{Int64: bundle.Size, Valid: true}
		extrasFormat = sql.NullString{String: bundle.Format, Valid: true}
		extrasGeneration = sql.NullInt64{Int64: req.Generation, Valid: true}
	}
	_, err = q.ReleaseLease(ctx, dbgen.ReleaseLeaseParams{
		SnapshotCommit:       sql.NullString{String: req.SnapshotCommit, Valid: req.SnapshotCommit != ""},
		BaseCommit:           sql.NullString{String: req.BaseCommit, Valid: req.BaseCommit != ""},
		AgentStateDigest:     agentDigest,
		AgentStateSize:       agentSize,
		AgentStateFormat:     agentFormat,
		AgentSessionID:       agentSession,
		AgentStateGeneration: agentGeneration,
		ExtrasDigest:         extrasDigest,
		ExtrasSize:           extrasSize,
		ExtrasFormat:         extrasFormat,
		ExtrasGeneration:     extrasGeneration,
		ID:                   lease.ID,
	})
	if err != nil {
		return err
	}

	// Directed handoff: grant to the target in the same transaction, so the
	// lease is never observably free in between. The target syncs on its
	// next take (idempotent), preserving the fast-forward invariant.
	if req.HandoffTo != "" {
		if req.HandoffTo == replica {
			return fmt.Errorf("%w: cannot hand off to yourself; use release", ErrConflict)
		}
		target, err := q.UpsertReplica(ctx, dbgen.UpsertReplicaParams{Name: req.HandoffTo})
		if err != nil {
			return err
		}
		_, err = q.GrantLease(ctx, dbgen.GrantLeaseParams{
			HolderReplicaID: sql.NullInt64{Int64: target.ID, Valid: true},
			PushToken:       sql.NullString{String: newPushToken(), Valid: true},
			ExpiresAt:       s.expiryAt(s.now().UnixMilli()),
			ID:              lease.ID,
		})
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *LeaseService) UploadAgentState(ctx context.Context, wsName, branch, replica string, generation int64, digest string, size int64, src io.Reader) error {
	return s.uploadBlob(ctx, wsName, branch, replica, generation, digest, size, protocol.MaxAgentStateBytes, "agent-state", src)
}

func (s *LeaseService) UploadExtras(ctx context.Context, wsName, branch, replica string, generation int64, digest string, size int64, src io.Reader) error {
	return s.uploadBlob(ctx, wsName, branch, replica, generation, digest, size, protocol.MaxExtrasBytes, "encrypted extras", src)
}

func (s *LeaseService) uploadBlob(ctx context.Context, wsName, branch, replica string, generation int64, digest string, size, maxSize int64, kind string, src io.Reader) error {
	if size < 0 || size > maxSize {
		return fmt.Errorf("%w: invalid %s upload size", ErrConflict, kind)
	}
	return s.git.withGCBarrier(func() error {
		ws, err := s.q.GetWorkspaceByName(ctx, wsName)
		if err != nil {
			return fmt.Errorf("%w: workspace %q", ErrNotFound, wsName)
		}
		rep, err := s.q.UpsertReplica(ctx, dbgen.UpsertReplicaParams{Name: replica})
		if err != nil {
			return err
		}
		lease, err := s.q.GetLease(ctx, dbgen.GetLeaseParams{WorkspaceID: ws.ID, Branch: branch})
		if err != nil {
			return fmt.Errorf("%w: no lease for branch %q", ErrNotFound, branch)
		}
		expired := lease.ExpiresAt.Valid && lease.ExpiresAt.Int64 <= s.now().UnixMilli()
		if lease.State != "held" || expired || !lease.HolderReplicaID.Valid || lease.HolderReplicaID.Int64 != rep.ID || lease.Generation != generation {
			return fmt.Errorf("%w: stale or unowned %s upload", ErrConflict, kind)
		}
		return s.blob.Put(ctx, digest, size, src)
	})
}

func (s *LeaseService) OpenExtras(ctx context.Context, wsName, branch, digest string) (*os.File, int64, error) {
	ws, err := s.q.GetWorkspaceByName(ctx, wsName)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: workspace %q", ErrNotFound, wsName)
	}
	lease, err := s.q.GetLease(ctx, dbgen.GetLeaseParams{WorkspaceID: ws.ID, Branch: branch})
	if err != nil || !lease.ExtrasDigest.Valid || lease.ExtrasDigest.String != digest || !lease.ExtrasSize.Valid {
		return nil, 0, fmt.Errorf("%w: encrypted extras bundle", ErrNotFound)
	}
	f, err := s.blob.Open(digest, lease.ExtrasSize.Int64)
	return f, lease.ExtrasSize.Int64, err
}

func (s *LeaseService) OpenAgentState(ctx context.Context, wsName, branch, digest string) (*os.File, int64, error) {
	ws, err := s.q.GetWorkspaceByName(ctx, wsName)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: workspace %q", ErrNotFound, wsName)
	}
	lease, err := s.q.GetLease(ctx, dbgen.GetLeaseParams{WorkspaceID: ws.ID, Branch: branch})
	if err != nil || !lease.AgentStateDigest.Valid || lease.AgentStateDigest.String != digest || !lease.AgentStateSize.Valid {
		return nil, 0, fmt.Errorf("%w: agent-state bundle", ErrNotFound)
	}
	f, err := s.blob.Open(digest, lease.AgentStateSize.Int64)
	return f, lease.AgentStateSize.Int64, err
}

// ValidatePush is called by the pre-receive hook. The token must belong to a
// currently-held lease in this workspace, and every pushed ref must belong to
// the leased branch (the branch head itself or its zync snapshot ref).
func (s *LeaseService) ValidatePush(ctx context.Context, wsName, token, refLines string) error {
	if token == "" {
		return fmt.Errorf("%w: missing push token", ErrConflict)
	}
	now, expiresAt := s.deadline()
	lease, err := s.q.RenewLeaseByPushToken(ctx, dbgen.RenewLeaseByPushTokenParams{
		NewExpiresAt: expiresAt,
		PushToken:    sql.NullString{String: token, Valid: true},
		Now:          sql.NullInt64{Int64: now, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("%w: unknown or expired push token", ErrConflict)
	}
	ws, err := s.q.GetWorkspaceByID(ctx, lease.WorkspaceID)
	if err != nil || ws.Name != wsName {
		return fmt.Errorf("%w: token belongs to a different workspace", ErrConflict)
	}
	if ws.ArchivedAt.Valid {
		return fmt.Errorf("%w: workspace %q is archived", ErrConflict, wsName)
	}
	allowed := map[string]bool{
		"refs/heads/" + lease.Branch:          true,
		"refs/zync/snapshots/" + lease.Branch: true,
	}
	for _, line := range strings.Split(strings.TrimSpace(refLines), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return fmt.Errorf("%w: malformed ref line", ErrConflict)
		}
		if !allowed[fields[2]] {
			return fmt.Errorf("%w: ref %q is not covered by the lease on branch %q", ErrConflict, fields[2], lease.Branch)
		}
	}
	return nil
}

func (s *LeaseService) Heartbeat(ctx context.Context, wsName, replica string, req protocol.HeartbeatRequest) (protocol.HeartbeatResponse, error) {
	if req.Branch == "" || replica == "" || req.PushToken == "" {
		return protocol.HeartbeatResponse{}, fmt.Errorf("%w: branch, replica, and push token are required", ErrConflict)
	}
	ws, err := s.q.GetWorkspaceByName(ctx, wsName)
	if err != nil {
		return protocol.HeartbeatResponse{}, fmt.Errorf("%w: workspace %q", ErrNotFound, wsName)
	}
	rep, err := s.q.UpsertReplica(ctx, dbgen.UpsertReplicaParams{Name: replica})
	if err != nil {
		return protocol.HeartbeatResponse{}, err
	}
	lease, err := s.q.GetLease(ctx, dbgen.GetLeaseParams{WorkspaceID: ws.ID, Branch: req.Branch})
	if err != nil {
		return protocol.HeartbeatResponse{}, fmt.Errorf("%w: no lease for branch %q", ErrNotFound, req.Branch)
	}
	now, expiresAt := s.deadline()
	renewed, err := s.q.RenewLease(ctx, dbgen.RenewLeaseParams{
		NewExpiresAt:    expiresAt,
		ID:              lease.ID,
		HolderReplicaID: sql.NullInt64{Int64: rep.ID, Valid: true},
		Generation:      req.Generation,
		PushToken:       sql.NullString{String: req.PushToken, Valid: true},
		Now:             sql.NullInt64{Int64: now, Valid: true},
	})
	if err != nil {
		return protocol.HeartbeatResponse{}, fmt.Errorf("%w: lease is stale or expired", ErrConflict)
	}
	return protocol.HeartbeatResponse{ExpiresAt: renewed.ExpiresAt.Int64, HeartbeatInterval: s.heartbeatInterval()}, nil
}

func (s *LeaseService) ListLeases(ctx context.Context) ([]protocol.LeaseInfo, error) {
	rows, err := s.q.ListActiveLeases(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.LeaseInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, leaseInfo(r.WorkspaceName, r.Branch, r.State, r.HolderName, r.HolderOpencodeUrl, r.HolderWorkspacesDir, r.Generation, r.SnapshotCommit, r.BaseCommit, r.ExpiresAt, r.UpdatedAt, s.now().UnixMilli()))
	}
	return out, nil
}

func (s *LeaseService) ListWorkspaces(ctx context.Context) ([]protocol.WorkspaceInfo, error) {
	wss, err := s.q.ListWorkspaces(ctx)
	return s.workspaceInfos(ctx, wss, err)
}

func (s *LeaseService) ListAllWorkspaces(ctx context.Context) ([]protocol.WorkspaceInfo, error) {
	wss, err := s.q.ListAllWorkspaces(ctx)
	return s.workspaceInfos(ctx, wss, err)
}

func (s *LeaseService) workspaceInfos(ctx context.Context, wss []dbgen.Workspace, err error) ([]protocol.WorkspaceInfo, error) {
	if err != nil {
		return nil, err
	}
	out := make([]protocol.WorkspaceInfo, 0, len(wss))
	for _, ws := range wss {
		info, err := s.workspaceInfo(ctx, ws)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

func (s *LeaseService) ArchiveWorkspace(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	ws, err := q.GetWorkspaceAnyByName(ctx, name)
	if err != nil {
		return fmt.Errorf("%w: workspace %q", ErrNotFound, name)
	}
	if ws.ArchivedAt.Valid {
		return tx.Commit()
	}
	rows, err := q.ListLeasesByWorkspace(ctx, ws.ID)
	if err != nil {
		return err
	}
	now := s.now().UnixMilli()
	for _, lease := range rows {
		live := lease.State == "held" && (!lease.ExpiresAt.Valid || lease.ExpiresAt.Int64 > now)
		if live {
			return fmt.Errorf("%w: workspace %q has a live lease on branch %q", ErrConflict, name, lease.Branch)
		}
	}
	if _, err := q.ArchiveWorkspace(ctx, dbgen.ArchiveWorkspaceParams{ArchivedAt: sql.NullInt64{Int64: now, Valid: true}, ID: ws.ID}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *LeaseService) RestoreWorkspace(ctx context.Context, name string) error {
	ws, err := s.q.GetWorkspaceAnyByName(ctx, name)
	if err != nil {
		return fmt.Errorf("%w: workspace %q", ErrNotFound, name)
	}
	if !ws.ArchivedAt.Valid {
		return nil
	}
	_, err = s.q.RestoreWorkspace(ctx, ws.ID)
	return err
}

func leaseInfo(workspace, branch, state string, holder, holderOC, holderWS sql.NullString, generation int64, snap, base sql.NullString, expiresAt sql.NullInt64, updatedAt string, now int64) protocol.LeaseInfo {
	if state == "held" && expiresAt.Valid && expiresAt.Int64 <= now {
		state = "expired"
	}
	return protocol.LeaseInfo{
		Workspace:           workspace,
		Branch:              branch,
		State:               state,
		Holder:              holder.String,
		HolderOpencodeURL:   holderOC.String,
		HolderWorkspacesDir: holderWS.String,
		Generation:          generation,
		SnapshotCommit:      snap.String,
		BaseCommit:          base.String,
		ExpiresAt:           expiresAt.Int64,
		UpdatedAt:           updatedAt,
	}
}

func (s *LeaseService) deadline() (int64, sql.NullInt64) {
	now := s.now().UnixMilli()
	return now, s.expiryAt(now)
}

func (s *LeaseService) expiryAt(now int64) sql.NullInt64 {
	if s.ttl == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: now + s.ttl.Milliseconds(), Valid: true}
}

func (s *LeaseService) heartbeatInterval() int64 {
	if s.ttl == 0 {
		return 0
	}
	seconds := int64(s.ttl / (3 * time.Second))
	if seconds < 1 {
		return 1
	}
	return seconds
}

func (s *LeaseService) takeResponse(lease dbgen.Lease) protocol.TakeResponse {
	return protocol.TakeResponse{
		Generation:        lease.Generation,
		PushToken:         lease.PushToken.String,
		SnapshotCommit:    lease.SnapshotCommit.String,
		BaseCommit:        lease.BaseCommit.String,
		AgentState:        agentStateBundle(lease.AgentStateDigest, lease.AgentStateSize, lease.AgentStateFormat, lease.AgentSessionID, lease.AgentStateGeneration),
		Extras:            extrasBundle(lease.ExtrasDigest, lease.ExtrasSize, lease.ExtrasFormat, lease.ExtrasGeneration),
		ExpiresAt:         lease.ExpiresAt.Int64,
		HeartbeatInterval: s.heartbeatInterval(),
	}
}

func extrasBundle(digest sql.NullString, size sql.NullInt64, format sql.NullString, generation sql.NullInt64) *protocol.ExtrasBundle {
	if !digest.Valid || !size.Valid || !format.Valid || !generation.Valid {
		return nil
	}
	return &protocol.ExtrasBundle{Digest: digest.String, Size: size.Int64, Format: format.String, SourceGeneration: generation.Int64}
}

func agentStateBundle(digest sql.NullString, size sql.NullInt64, format, session sql.NullString, generation sql.NullInt64) *protocol.AgentStateBundle {
	if !digest.Valid || !size.Valid || !format.Valid || !session.Valid || !generation.Valid {
		return nil
	}
	return &protocol.AgentStateBundle{
		Digest:           digest.String,
		Size:             size.Int64,
		Format:           format.String,
		SessionID:        session.String,
		SourceGeneration: generation.Int64,
	}
}

func validSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
