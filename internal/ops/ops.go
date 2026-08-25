// Package ops implements the client side of the handoff protocol. Both the
// CLI subcommands and the TUI call into this package.
package ops

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/notzree/zync/internal/client"
	"github.com/notzree/zync/internal/cliconf"
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
	return &Ops{Global: g, Client: client.New(g.HubURL, g.Token, g.Replica, g.OpencodeURL, g.WorkspacesDir)}
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
	state.Branches[branch] = &cliconf.BranchState{Holding: true, Generation: resp.Generation, PushToken: resp.PushToken}
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
	if err := cliconf.SaveRegistryEntry(workspace, abs); err != nil {
		return "", err
	}
	_ = ws
	return abs, nil
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

	if bs := state.Branches[branch]; bs != nil && bs.Holding {
		return nil // already holding; idempotent
	}

	// Enforce sync-before-write: the worktree must be exactly what zync
	// last recorded before we overwrite it with the new snapshot.
	if _, err := verifyWorktree(repo, state); err != nil {
		return err
	}

	resp, err := o.Client.Take(state.Workspace, branch, force)
	if err != nil {
		return err
	}

	if resp.SnapshotCommit == "" {
		// Fresh lease: no flushed state exists anywhere, nothing to sync.
		if branch != cur {
			return fmt.Errorf("lease on %q granted, but it has no synced state on the hub; check the branch out manually and re-run `zync take`", branch)
		}
	} else {
		if err := repo.FetchBranch(branch); err != nil {
			return err
		}
		if repo.BranchExists(branch) && !repo.IsAncestor(branch, resp.BaseCommit) {
			return fmt.Errorf("%w: local branch %q has commits the hub does not know about; reconcile manually", ErrDiverged, branch)
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
		if err := repo.Restore(branch, norm, resp.SnapshotCommit, resp.BaseCommit); err != nil {
			return err
		}
		tree, err := repo.TreeOf(resp.SnapshotCommit)
		if err != nil {
			return err
		}
		state.Worktree = &cliconf.WorktreeState{Branch: branch, Tree: tree, Commit: resp.SnapshotCommit}
	}

	state.Branches[branch] = &cliconf.BranchState{
		Holding:        true,
		Generation:     resp.Generation,
		PushToken:      resp.PushToken,
		SnapshotCommit: resp.SnapshotCommit,
		BaseCommit:     resp.BaseCommit,
	}
	return cliconf.SaveRepoState(repo.GitDir, state)
}

// Release flushes the full working state to the hub and releases the lease
// back to the pool (nobody holds it). The working tree is left untouched.
func (o *Ops) Release(dir, branch string) error {
	return o.flushAndRelease(dir, branch, "")
}

// Handoff flushes and atomically transfers the lease to target. With an
// empty target, it auto-selects when exactly one other replica advertises an
// opencode server.
func (o *Ops) Handoff(dir, branch, target string) (string, error) {
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
	return target, o.flushAndRelease(dir, branch, target)
}

func (o *Ops) flushAndRelease(dir, branch, target string) error {
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

	// Phase 1: flush. If anything here fails, the lease is retained.
	snapshot, tree, err := repo.Snapshot(branch)
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

	// Phase 2: release (optionally granting straight to a target replica).
	// Generation fencing rejects stale holders.
	err = o.Client.Release(state.Workspace, protocol.ReleaseRequest{
		Branch:         branch,
		Generation:     bs.Generation,
		SnapshotCommit: snapshot,
		BaseCommit:     head,
		HandoffTo:      target,
	})
	if err != nil {
		return fmt.Errorf("release failed; lease retained: %w", err)
	}

	state.Branches[branch] = &cliconf.BranchState{
		Holding:        false,
		Generation:     bs.Generation,
		SnapshotCommit: snapshot,
		BaseCommit:     head,
	}
	state.Worktree = &cliconf.WorktreeState{Branch: branch, Tree: tree, Commit: snapshot}
	return cliconf.SaveRepoState(repo.GitDir, state)
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
		ls.Holding = bs.Holding
	}
	if !ls.Holding {
		if _, err := verifyWorktree(repo, state); errors.Is(err, ErrDiverged) {
			ls.Diverged = true
		}
	}
	return ls, nil
}
