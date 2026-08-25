package hub

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

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
	db  *sql.DB
	q   *dbgen.Queries
	git *GitManager
}

func NewLeaseService(conn *sql.DB, git *GitManager) *LeaseService {
	return &LeaseService{db: conn, q: dbgen.New(conn), git: git}
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
	if _, err := s.q.GetWorkspaceByName(ctx, name); err == nil {
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
	rows, err := s.q.ListLeasesByWorkspace(ctx, ws.ID)
	if err != nil {
		return protocol.WorkspaceInfo{}, err
	}
	info := protocol.WorkspaceInfo{Name: ws.Name, DefaultBranch: ws.DefaultBranch}
	for _, r := range rows {
		info.Leases = append(info.Leases, leaseInfo(r.WorkspaceName, r.Branch, r.State, r.HolderName, r.HolderOpencodeUrl, r.HolderWorkspacesDir, r.Generation, r.SnapshotCommit, r.BaseCommit, r.UpdatedAt))
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

	lease, err := q.GetLease(ctx, dbgen.GetLeaseParams{WorkspaceID: ws.ID, Branch: branch})
	if errors.Is(err, sql.ErrNoRows) {
		created, err := q.CreateHeldLease(ctx, dbgen.CreateHeldLeaseParams{
			WorkspaceID:     ws.ID,
			Branch:          branch,
			HolderReplicaID: sql.NullInt64{Int64: rep.ID, Valid: true},
			PushToken:       sql.NullString{String: newPushToken(), Valid: true},
		})
		if err != nil {
			return protocol.TakeResponse{}, err
		}
		if err := tx.Commit(); err != nil {
			return protocol.TakeResponse{}, err
		}
		return protocol.TakeResponse{Generation: created.Generation, PushToken: created.PushToken.String}, nil
	}
	if err != nil {
		return protocol.TakeResponse{}, err
	}

	if lease.State == "held" {
		if lease.HolderReplicaID.Valid && lease.HolderReplicaID.Int64 == rep.ID {
			if err := tx.Commit(); err != nil {
				return protocol.TakeResponse{}, err
			}
			return protocol.TakeResponse{
				Generation:     lease.Generation,
				PushToken:      lease.PushToken.String,
				SnapshotCommit: lease.SnapshotCommit.String,
				BaseCommit:     lease.BaseCommit.String,
			}, nil
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
		ID:              lease.ID,
	})
	if err != nil {
		return protocol.TakeResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.TakeResponse{}, err
	}
	return protocol.TakeResponse{
		Generation:     granted.Generation,
		PushToken:      granted.PushToken.String,
		SnapshotCommit: granted.SnapshotCommit.String,
		BaseCommit:     granted.BaseCommit.String,
	}, nil
}

// Release records the flush (snapshot + base commits) and frees the lease.
// The generation must match the caller's grant: a stale holder that was
// force-broken cannot release over a newer grant.
func (s *LeaseService) Release(ctx context.Context, wsName string, req protocol.ReleaseRequest, replica string) error {
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
	_, err = q.ReleaseLease(ctx, dbgen.ReleaseLeaseParams{
		SnapshotCommit: sql.NullString{String: req.SnapshotCommit, Valid: req.SnapshotCommit != ""},
		BaseCommit:     sql.NullString{String: req.BaseCommit, Valid: req.BaseCommit != ""},
		ID:             lease.ID,
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
			ID:              lease.ID,
		})
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ValidatePush is called by the pre-receive hook. The token must belong to a
// currently-held lease in this workspace, and every pushed ref must belong to
// the leased branch (the branch head itself or its zync snapshot ref).
func (s *LeaseService) ValidatePush(ctx context.Context, wsName, token, refLines string) error {
	if token == "" {
		return fmt.Errorf("%w: missing push token", ErrConflict)
	}
	row, err := s.q.GetHeldLeaseByPushToken(ctx, sql.NullString{String: token, Valid: true})
	if err != nil {
		return fmt.Errorf("%w: unknown or expired push token", ErrConflict)
	}
	if row.WorkspaceName != wsName {
		return fmt.Errorf("%w: token belongs to a different workspace", ErrConflict)
	}
	allowed := map[string]bool{
		"refs/heads/" + row.Branch:          true,
		"refs/zync/snapshots/" + row.Branch: true,
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
			return fmt.Errorf("%w: ref %q is not covered by the lease on branch %q", ErrConflict, fields[2], row.Branch)
		}
	}
	return nil
}

func (s *LeaseService) ListLeases(ctx context.Context) ([]protocol.LeaseInfo, error) {
	rows, err := s.q.ListAllLeases(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.LeaseInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, leaseInfo(r.WorkspaceName, r.Branch, r.State, r.HolderName, r.HolderOpencodeUrl, r.HolderWorkspacesDir, r.Generation, r.SnapshotCommit, r.BaseCommit, r.UpdatedAt))
	}
	return out, nil
}

func (s *LeaseService) ListWorkspaces(ctx context.Context) ([]protocol.WorkspaceInfo, error) {
	wss, err := s.q.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.WorkspaceInfo, 0, len(wss))
	for _, ws := range wss {
		info, err := s.GetWorkspace(ctx, ws.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

func leaseInfo(workspace, branch, state string, holder, holderOC, holderWS sql.NullString, generation int64, snap, base sql.NullString, updatedAt string) protocol.LeaseInfo {
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
		UpdatedAt:           updatedAt,
	}
}
