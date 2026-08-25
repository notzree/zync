// Package ops implements the client side of the handoff protocol. Both the
// CLI subcommands and the TUI call into this package.
package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/notzree/zync/internal/agentstate"
	"github.com/notzree/zync/internal/cliconf"
	"github.com/notzree/zync/internal/client"
	"github.com/notzree/zync/internal/extras"
	"github.com/notzree/zync/internal/gitx"
	"github.com/notzree/zync/internal/protocol"
)

// ErrDiverged means the working tree was mutated without holding the lease.
// Nothing is destroyed; the user must reconcile manually.
var ErrDiverged = errors.New("diverged")

type Ops struct {
	Global cliconf.Global
	Client *client.Client
}

func New(g cliconf.Global) *Ops {
	return &Ops{Global: g, Client: client.New(g.HubURL, g.Token, g.Replica, g.Kind, g.OpencodeURL, g.WorkspacesDir)}
}

func (o *Ops) openRepo(dir string) (*gitx.Repo, *cliconf.RepoState, error) {
	repo, err := gitx.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	repo.SetAuth(o.Global.Token)
	state, err := cliconf.LoadRepoState(repo.GitDir)
	if err != nil {
		return nil, nil, err
	}
	return repo, state, nil
}

// verifyWorktree enforces the read-only invariant: the working tree must
// match what zync last left there (or be clean if zync has no record).
func verifyWorktree(repo *gitx.Repo, state *cliconf.RepoState) (currentTree string, err error) {
	tree, err := repo.WorktreeTree()
	if err != nil {
		return "", err
	}
	expected := ""
	if state != nil && state.Worktree != nil {
		expected = state.Worktree.Tree
	} else {
		expected, err = repo.TreeOf("HEAD")
		if err != nil {
			return "", err
		}
	}
	if tree != expected {
		return "", fmt.Errorf("%w: the working tree was modified without holding the lease; commit or stash your changes to a rescue branch, or take the lease with --force and reconcile", ErrDiverged)
	}
	indexTree, err := repo.IndexTree()
	if err != nil {
		return "", err
	}
	expectedIndex := ""
	if state != nil && state.Worktree != nil {
		expectedIndex = state.Worktree.IndexTree
	} else {
		expectedIndex = expected
	}
	if expectedIndex != "" && indexTree != expectedIndex {
		return "", fmt.Errorf("%w: the Git index was modified without holding the lease; restore the expected staging state or take the lease with --force and reconcile", ErrDiverged)
	}
	return tree, nil
}

// Init enrolls the current repository as a new workspace and takes the lease
// on the current branch.
func (o *Ops) Init(dir, workspace string) (string, error) {
	repo, _, err := o.openRepo(dir)
	if err != nil {
		return "", err
	}
	if workspace == "" {
		workspace = filepath.Base(repo.Dir)
	}
	branch, err := repo.CurrentBranch()
	if err != nil {
		return "", err
	}
	if _, err := repo.RevParse("HEAD"); err != nil {
		return "", errors.New("repository has no commits yet; make an initial commit first")
	}

	if _, err := o.Client.CreateWorkspace(workspace, branch); err != nil {
		return "", err
	}
	if err := repo.EnsureRemote(o.Client.GitURL(workspace)); err != nil {
		return "", err
	}
	resp, err := o.Client.Take(workspace, branch, false)
	if err != nil {
		return "", err
	}
	if err := repo.PushBranch(branch, resp.PushToken, false); err != nil {
		return "", err
	}

	state := &cliconf.RepoState{Workspace: workspace, Branches: map[string]*cliconf.BranchState{}}
	state.Branches[branch] = &cliconf.BranchState{Holding: true, Generation: resp.Generation, PushToken: resp.PushToken, ExpiresAt: resp.ExpiresAt}
	if err := cliconf.SaveRepoState(repo.GitDir, state); err != nil {
		return "", err
	}
	if err := cliconf.SaveRegistryEntry(workspace, repo.Dir); err != nil {
		return "", err
	}
	return workspace, nil
}

// Clone materializes an existing workspace onto this replica.
func (o *Ops) Clone(workspace, path string) (string, error) {
	return o.clone(workspace, path, true)
}

func (o *Ops) clone(workspace, path string, register bool) (string, error) {
	ws, err := o.Client.GetWorkspace(workspace)
	if err != nil {
		return "", err
	}
	if path == "" {
		path = workspace
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	tmp := &gitx.Repo{Dir: filepath.Dir(abs)}
	tmp.SetAuth(o.Global.Token)
	if _, err := tmp.RunRemote("clone", "--origin", gitx.RemoteName, o.Client.GitURL(workspace), abs); err != nil {
		return "", err
	}
	repo, err := gitx.Open(abs)
	if err != nil {
		return "", err
	}
	state := &cliconf.RepoState{Workspace: workspace, Branches: map[string]*cliconf.BranchState{}}
	if err := cliconf.SaveRepoState(repo.GitDir, state); err != nil {
		return "", err
	}
	if register {
		if err := cliconf.SaveRegistryEntry(workspace, abs); err != nil {
			return "", err
		}
	}
	_ = ws
	return abs, nil
}

// SyncWorkspaces atomically materializes newly enrolled active workspaces under
// root. Existing directories are accepted only when they are matching zync
// checkouts; unknown or partial directories are never removed automatically.
func (o *Ops) SyncWorkspaces(root string) error {
	if root == "" {
		return errors.New("workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	lock := filepath.Join(abs, ".zync-sync.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("another workspace sync is already running")
		}
		return err
	}
	defer os.Remove(lock)

	workspaces, err := o.Client.ListWorkspaces()
	if err != nil {
		return err
	}
	var errs []error
	for _, ws := range workspaces {
		final := filepath.Join(abs, ws.Name)
		if filepath.Dir(final) != abs {
			errs = append(errs, fmt.Errorf("unsafe workspace name %q", ws.Name))
			continue
		}
		if info, err := os.Lstat(final); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				errs = append(errs, fmt.Errorf("workspace %q destination is a symlink", ws.Name))
				continue
			}
			repo, state, openErr := o.openRepo(final)
			if openErr != nil || state == nil || state.Workspace != ws.Name {
				errs = append(errs, fmt.Errorf("workspace %q destination exists but is not its enrolled checkout", ws.Name))
				continue
			}
			if err := cliconf.SaveRegistryEntry(ws.Name, repo.Dir); err != nil {
				errs = append(errs, err)
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}

		tmp, err := os.MkdirTemp(abs, ".zync-clone-"+ws.Name+"-*")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		checkout := filepath.Join(tmp, "checkout")
		_, cloneErr := o.clone(ws.Name, checkout, false)
		if cloneErr == nil {
			cloneErr = os.Rename(checkout, final)
		}
		os.RemoveAll(tmp)
		if cloneErr != nil {
			errs = append(errs, fmt.Errorf("clone workspace %q: %w", ws.Name, cloneErr))
			continue
		}
		if err := cliconf.SaveRegistryEntry(ws.Name, final); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Take acquires the lease on branch and fast-forwards this replica to the
// latest flushed state, restoring uncommitted changes as uncommitted.
func (o *Ops) Take(dir, branch string, force bool) error {
	repo, state, err := o.openRepo(dir)
	if err != nil {
		return err
	}
	if state == nil {
		return errors.New("this repository is not enrolled; run `zync init` or `zync clone` first")
	}
	cur, err := repo.CurrentBranch()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cur
	}

	bs := state.Branches[branch]
	if bs != nil && bs.Holding {
		if _, err := o.renewHeldLease(state, branch); err != nil {
			_ = cliconf.SaveRepoState(repo.GitDir, state)
			return err
		}
		return cliconf.SaveRepoState(repo.GitDir, state)
	}

	// Enforce sync-before-write: the worktree must be exactly what zync
	// last recorded before we overwrite it with the new snapshot. With
	// --force, a diverged local state is adopted as the new truth instead
	// (snapshotted first, so nothing can be lost).
	_, verifyErr := verifyWorktree(repo, state)
	if verifyErr != nil && (!force || !errors.Is(verifyErr, ErrDiverged)) {
		return verifyErr
	}

	// adoptLocal makes the current local state authoritative: the next flush
	// pushes it to the hub (which fails safely if histories truly diverged).
	adoptLocal := func() error {
		if branch != cur {
			return fmt.Errorf("cannot adopt diverged local state for %q while on %q", branch, cur)
		}
		snap, tree, indexTree, err := repo.Snapshot(branch)
		if err != nil {
			return err
		}
		state.Worktree = &cliconf.WorktreeState{Branch: branch, Tree: tree, IndexTree: indexTree, Commit: snap}
		return nil
	}

	resp, err := o.Client.Take(state.Workspace, branch, force)
	if err != nil {
		return err
	}

	var restoredAgentState *protocol.AgentStateBundle
	var restoredExtras *protocol.ExtrasBundle
	switch {
	case verifyErr != nil:
		// Forced take over local divergence: local wins.
		if err := adoptLocal(); err != nil {
			return err
		}
	case resp.SnapshotCommit == "":
		// Fresh lease: no flushed state exists anywhere, nothing to sync.
		if branch != cur {
			return fmt.Errorf("lease on %q granted, but it has no synced state on the hub; check the branch out manually and re-run `zync take`", branch)
		}
	default:
		if err := repo.FetchBranch(branch); err != nil {
			return err
		}
		if repo.BranchExists(branch) && !repo.IsAncestor(branch, resp.BaseCommit) {
			if !force {
				return fmt.Errorf("%w: local branch %q has commits the hub does not know about; reconcile manually or take --force to make local state authoritative", ErrDiverged, branch)
			}
			if err := adoptLocal(); err != nil {
				return err
			}
			break
		}
		norm := ""
		if state.Worktree != nil {
			norm = state.Worktree.Commit
		}
		if norm == "" {
			if norm, err = repo.RevParse("HEAD"); err != nil {
				return err
			}
		}
		indexTree, err := repo.SnapshotIndexTree(resp.SnapshotCommit)
		if err != nil {
			return err
		}
		if resp.AgentState != nil && (bs == nil || bs.AgentStateDigest != resp.AgentState.Digest) {
			data, err := o.Client.DownloadAgentState(state.Workspace, branch, *resp.AgentState)
			if err != nil {
				return fmt.Errorf("download agent state: %w", err)
			}
			if err := agentstate.Import(context.Background(), repo.Dir, repo.GitDir, *resp.AgentState, data); err != nil {
				return fmt.Errorf("restore agent state: %w", err)
			}
		}
		var extrasData []byte
		if resp.Extras != nil && (bs == nil || bs.ExtrasDigest != resp.Extras.Digest) {
			extrasData, err = o.Client.DownloadExtras(state.Workspace, branch, *resp.Extras)
			if err != nil {
				return fmt.Errorf("download encrypted extras: %w", err)
			}
		}
		if err := repo.Restore(branch, norm, resp.SnapshotCommit, resp.BaseCommit, indexTree); err != nil {
			return err
		}
		tree, err := repo.TreeOf(resp.SnapshotCommit)
		if err != nil {
			return err
		}
		state.Worktree = &cliconf.WorktreeState{Branch: branch, Tree: tree, IndexTree: indexTree, Commit: resp.SnapshotCommit}
		restoredAgentState = resp.AgentState
		if extrasData != nil {
			if err := extras.Import(context.Background(), repo.Dir, *resp.Extras, extrasData); err != nil {
				state.Branches[branch] = &cliconf.BranchState{
					Holding: false, Generation: resp.Generation, SnapshotCommit: resp.SnapshotCommit,
					BaseCommit: resp.BaseCommit, ExpiresAt: resp.ExpiresAt,
				}
				_ = cliconf.SaveRepoState(repo.GitDir, state)
				return fmt.Errorf("restore encrypted extras: %w", err)
			}
		}
		restoredExtras = resp.Extras
	}

	branchState := &cliconf.BranchState{
		Holding:        true,
		Generation:     resp.Generation,
		PushToken:      resp.PushToken,
		SnapshotCommit: resp.SnapshotCommit,
		BaseCommit:     resp.BaseCommit,
		ExpiresAt:      resp.ExpiresAt,
	}
	if restoredAgentState != nil {
		branchState.AgentStateDigest = restoredAgentState.Digest
		branchState.AgentSessionID = restoredAgentState.SessionID
	}
	if restoredExtras != nil {
		branchState.ExtrasDigest = restoredExtras.Digest
	}
	state.Branches[branch] = branchState
	return cliconf.SaveRepoState(repo.GitDir, state)
}

// Release flushes the full working state to the hub and releases the lease
// back to the pool (nobody holds it). The working tree is left untouched.
func (o *Ops) Release(dir, branch string) error {
	return o.flushAndRelease(dir, branch, "", "")
}

func (o *Ops) ReleaseWithAgentSession(dir, branch, sessionID string) error {
	return o.flushAndRelease(dir, branch, "", sessionID)
}

// Handoff flushes and atomically transfers the lease to target. With an
// empty target, it auto-selects when exactly one other replica advertises an
// opencode server.
func (o *Ops) Handoff(dir, branch, target string) (string, error) {
	return o.handoff(dir, branch, target, "")
}

func (o *Ops) HandoffWithAgentSession(dir, branch, target, sessionID string) (string, error) {
	return o.handoff(dir, branch, target, sessionID)
}

func (o *Ops) handoff(dir, branch, target, sessionID string) (string, error) {
	if target == "" {
		replicas, err := o.Client.ListReplicas()
		if err != nil {
			return "", err
		}
		var candidates []string
		for _, r := range replicas {
			if r.OpencodeURL != "" && r.Name != o.Global.Replica {
				candidates = append(candidates, r.Name)
			}
		}
		switch len(candidates) {
		case 1:
			target = candidates[0]
		case 0:
			return "", errors.New("no target: no other replica advertises an opencode server; use --to <replica> or `zync release`")
		default:
			return "", fmt.Errorf("ambiguous target, use --to <replica> (candidates: %s)", strings.Join(candidates, ", "))
		}
	}
	if target == o.Global.Replica {
		return "", errors.New("cannot hand off to yourself; use `zync release`")
	}
	return target, o.flushAndRelease(dir, branch, target, sessionID)
}

func (o *Ops) flushAndRelease(dir, branch, target, agentSessionID string) error {
	repo, state, err := o.openRepo(dir)
	if err != nil {
		return err
	}
	if state == nil {
		return errors.New("this repository is not enrolled; run `zync init` or `zync clone` first")
	}
	cur, err := repo.CurrentBranch()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cur
	}
	if branch != cur {
		return fmt.Errorf("release/handoff must run from the branch being flushed (on %q, asked for %q)", cur, branch)
	}
	bs := state.Branches[branch]
	if bs == nil || !bs.Holding {
		return fmt.Errorf("this replica does not hold the lease on %q", branch)
	}
	if _, err := o.renewHeldLease(state, branch); err != nil {
		_ = cliconf.SaveRepoState(repo.GitDir, state)
		return fmt.Errorf("renew lease before flush: %w", err)
	}
	if err := cliconf.SaveRepoState(repo.GitDir, state); err != nil {
		return err
	}
	bs = state.Branches[branch]
	var bundle *protocol.AgentStateBundle
	var bundleData []byte
	var extrasBundle *protocol.ExtrasBundle
	var extrasData []byte
	if agentSessionID != "" {
		exported, data, err := agentstate.Export(context.Background(), repo.Dir, agentSessionID)
		if err != nil {
			return fmt.Errorf("export agent state: %w", err)
		}
		exported.SourceGeneration = bs.Generation
		bundle = &exported
		bundleData = data
	}
	extrasBundle, extrasData, err = extras.Export(context.Background(), repo.Dir)
	if err != nil {
		return fmt.Errorf("export encrypted extras: %w", err)
	}
	if extrasBundle != nil {
		extrasBundle.SourceGeneration = bs.Generation
	}

	// Phase 1: flush. If anything here fails, the lease is retained.
	snapshot, tree, indexTree, err := repo.Snapshot(branch)
	if err != nil {
		return err
	}
	head, err := repo.RevParse("HEAD")
	if err != nil {
		return err
	}
	if err := repo.PushBranch(branch, bs.PushToken, true); err != nil {
		return fmt.Errorf("flush push failed; lease retained: %w", err)
	}
	if bundle != nil {
		if err := o.Client.UploadAgentState(state.Workspace, branch, bs.Generation, *bundle, bundleData); err != nil {
			return fmt.Errorf("agent-state upload failed; lease retained: %w", err)
		}
	}
	if extrasBundle != nil {
		if err := o.Client.UploadExtras(state.Workspace, branch, bs.Generation, *extrasBundle, extrasData); err != nil {
			return fmt.Errorf("encrypted extras upload failed; lease retained: %w", err)
		}
	}

	// Phase 2: release (optionally granting straight to a target replica).
	// Generation fencing rejects stale holders.
	err = o.Client.Release(state.Workspace, protocol.ReleaseRequest{
		Branch:         branch,
		Generation:     bs.Generation,
		SnapshotCommit: snapshot,
		BaseCommit:     head,
		AgentState:     bundle,
		Extras:         extrasBundle,
		HandoffTo:      target,
	})
	if err != nil {
		return fmt.Errorf("release failed; lease retained: %w", err)
	}

	branchState := &cliconf.BranchState{
		Holding:        false,
		Generation:     bs.Generation,
		SnapshotCommit: snapshot,
		BaseCommit:     head,
	}
	if bundle != nil {
		branchState.AgentStateDigest = bundle.Digest
		branchState.AgentSessionID = bundle.SessionID
	}
	if extrasBundle != nil {
		branchState.ExtrasDigest = extrasBundle.Digest
	}
	state.Branches[branch] = branchState
	state.Worktree = &cliconf.WorktreeState{Branch: branch, Tree: tree, IndexTree: indexTree, Commit: snapshot}
	return cliconf.SaveRepoState(repo.GitDir, state)
}

func (o *Ops) Heartbeat(dir, branch string) (time.Duration, error) {
	repo, state, err := o.openRepo(dir)
	if err != nil {
		return 0, err
	}
	if state == nil {
		return 0, errors.New("this repository is not enrolled")
	}
	if branch == "" {
		branch, err = repo.CurrentBranch()
		if err != nil {
			return 0, err
		}
	}
	interval, err := o.renewHeldLease(state, branch)
	if saveErr := cliconf.SaveRepoState(repo.GitDir, state); err == nil && saveErr != nil {
		err = saveErr
	}
	return interval, err
}

func (o *Ops) renewHeldLease(state *cliconf.RepoState, branch string) (time.Duration, error) {
	bs := state.Branches[branch]
	if bs == nil || !bs.Holding || bs.PushToken == "" {
		return 0, fmt.Errorf("this replica does not hold the lease on %q", branch)
	}
	resp, err := o.Client.Heartbeat(state.Workspace, protocol.HeartbeatRequest{
		Branch: branch, Generation: bs.Generation, PushToken: bs.PushToken,
	})
	if err == nil {
		bs.ExpiresAt = resp.ExpiresAt
		return heartbeatDuration(resp.HeartbeatInterval), nil
	}
	if !errors.Is(err, client.ErrConflict) {
		return 0, err
	}

	// An expired holder can reclaim normally without losing its local work, but
	// only when the immediately next generation still names the same durable
	// snapshot. Any intervening holder requires explicit reconciliation.
	reclaimed, takeErr := o.Client.Take(state.Workspace, branch, false)
	if takeErr != nil {
		bs.Holding = false
		bs.PushToken = ""
		bs.ExpiresAt = 0
		return 0, fmt.Errorf("%w: lease expired and could not be reclaimed: %v", ErrDiverged, takeErr)
	}
	if reclaimed.Generation != bs.Generation+1 || reclaimed.SnapshotCommit != bs.SnapshotCommit || reclaimed.BaseCommit != bs.BaseCommit {
		bs.Holding = false
		bs.PushToken = ""
		bs.ExpiresAt = 0
		return 0, fmt.Errorf("%w: another holder advanced the lease after this replica expired", ErrDiverged)
	}
	bs.Generation = reclaimed.Generation
	bs.PushToken = reclaimed.PushToken
	bs.ExpiresAt = reclaimed.ExpiresAt
	return heartbeatDuration(reclaimed.HeartbeatInterval), nil
}

func heartbeatDuration(seconds int64) time.Duration {
	if seconds <= 0 {
		return time.Minute
	}
	return time.Duration(seconds) * time.Second
}

// LocalStatus describes this replica's view of the repo at dir.
type LocalStatus struct {
	Workspace string
	Branch    string
	Holding   bool
	Diverged  bool
}

func (o *Ops) LocalStatus(dir string) (*LocalStatus, error) {
	repo, state, err := o.openRepo(dir)
	if err != nil || state == nil {
		return nil, err
	}
	branch, err := repo.CurrentBranch()
	if err != nil {
		return nil, err
	}
	ls := &LocalStatus{Workspace: state.Workspace, Branch: branch}
	if bs := state.Branches[branch]; bs != nil {
		ls.Holding = bs.Holding && (bs.ExpiresAt == 0 || bs.ExpiresAt > time.Now().UnixMilli())
	}
	if !ls.Holding {
		if _, err := verifyWorktree(repo, state); errors.Is(err, ErrDiverged) {
			ls.Diverged = true
		}
	}
	return ls, nil
}

func (o *Ops) AgentSession(dir, branch string) (string, error) {
	repo, state, err := o.openRepo(dir)
	if err != nil || state == nil {
		return "", err
	}
	if branch == "" {
		branch, err = repo.CurrentBranch()
		if err != nil {
			return "", err
		}
	}
	if bs := state.Branches[branch]; bs != nil {
		return bs.AgentSessionID, nil
	}
	return "", nil
}
