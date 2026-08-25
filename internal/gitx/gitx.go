// Package gitx wraps the system git binary with the plumbing zync needs:
// snapshot commits of the full working state and conflict-free restores.
package gitx

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SnapshotRefPrefix is where flush snapshots live, keeping real branch
// history clean.
const SnapshotRefPrefix = "refs/zync/snapshots/"

// RemoteName is the dedicated remote pointing at the hub; the user's origin
// is never touched.
const RemoteName = "zync"

type Repo struct {
	Dir     string
	GitDir  string
	authEnv []string
}

// Open locates the repository containing dir.
func Open(dir string) (*Repo, error) {
	r := &Repo{Dir: dir}
	top, err := r.Run("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not inside a git repository: %w", err)
	}
	r.Dir = top
	gitDir, err := r.Run("rev-parse", "--git-dir")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(top, gitDir)
	}
	r.GitDir = gitDir
	return r, nil
}

// SetAuth configures the Authorization header used for all hub remote
// operations, passed via git config environment variables so the token never
// appears in process argv.
func (r *Repo) SetAuth(token string) {
	r.authEnv = []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Bearer " + token,
	}
}

func (r *Repo) run(extraEnv []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Repo) Run(args ...string) (string, error)       { return r.run(nil, args...) }
func (r *Repo) RunRemote(args ...string) (string, error) { return r.run(r.authEnv, args...) }

func (r *Repo) CurrentBranch() (string, error) {
	out, err := r.Run("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if out == "HEAD" {
		return "", fmt.Errorf("detached HEAD; check out a branch first")
	}
	return out, nil
}

func (r *Repo) RevParse(ref string) (string, error) {
	return r.Run("rev-parse", "--verify", ref+"^{commit}")
}

func (r *Repo) TreeOf(commitish string) (string, error) {
	return r.Run("rev-parse", commitish+"^{tree}")
}

func (r *Repo) BranchExists(branch string) bool {
	_, err := r.Run("show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func (r *Repo) IsAncestor(ancestor, descendant string) bool {
	_, err := r.Run("merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

func (r *Repo) EnsureRemote(url string) error {
	if _, err := r.Run("remote", "get-url", RemoteName); err == nil {
		_, err = r.Run("remote", "set-url", RemoteName, url)
		return err
	}
	_, err := r.Run("remote", "add", RemoteName, url)
	return err
}

var identEnv = []string{
	"GIT_AUTHOR_NAME=zync", "GIT_AUTHOR_EMAIL=zync@localhost",
	"GIT_COMMITTER_NAME=zync", "GIT_COMMITTER_EMAIL=zync@localhost",
}

// WorktreeTree computes the tree hash of the current working state (tracked
// changes plus untracked, non-ignored files) without touching the user's
// index. This is how divergence is detected: two identical states always
// hash to the same tree.
func (r *Repo) WorktreeTree() (string, error) {
	tree, _, err := r.writeWorktreeTree()
	return tree, err
}

// IndexTree returns the tree represented by the user's real Git index without
// modifying it. Git rejects write-tree when the index contains unmerged paths,
// so conflicted states fail the handoff rather than being flattened.
func (r *Repo) IndexTree() (string, error) {
	return r.run(identEnv, "write-tree")
}

func (r *Repo) writeWorktreeTree() (tree string, cleanup func(), err error) {
	tmp, err := os.CreateTemp(r.GitDir, "zync-index-*")
	if err != nil {
		return "", nil, err
	}
	tmp.Close()
	os.Remove(tmp.Name()) // git wants to create it itself
	cleanup = func() { os.Remove(tmp.Name()) }
	env := append([]string{"GIT_INDEX_FILE=" + tmp.Name()}, identEnv...)

	if _, err := r.run(env, "read-tree", "HEAD"); err != nil {
		cleanup()
		return "", nil, err
	}
	if _, err := r.run(env, "add", "-A", "."); err != nil {
		cleanup()
		return "", nil, err
	}
	tree, err = r.run(env, "write-tree")
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return tree, cleanup, nil
}

// Snapshot records both the full working tree and the real index. The index is
// encoded as a second-parent metadata commit so the existing snapshot ref
// transports both trees without another ref or protocol field.
func (r *Repo) Snapshot(branch string) (commit, tree, indexTree string, err error) {
	tree, cleanup, err := r.writeWorktreeTree()
	if err != nil {
		return "", "", "", err
	}
	defer cleanup()

	head, err := r.RevParse("HEAD")
	if err != nil {
		return "", "", "", err
	}
	indexTree, err = r.IndexTree()
	if err != nil {
		return "", "", "", err
	}
	indexCommit, err := r.run(identEnv, "commit-tree", indexTree, "-p", head, "-m", "zync index of "+branch)
	if err != nil {
		return "", "", "", err
	}
	commit, err = r.run(identEnv, "commit-tree", tree, "-p", head, "-p", indexCommit, "-m", "zync snapshot v2 of "+branch)
	if err != nil {
		return "", "", "", err
	}
	if _, err := r.Run("update-ref", SnapshotRefPrefix+branch, commit); err != nil {
		return "", "", "", err
	}
	return commit, tree, indexTree, nil
}

// SnapshotIndexTree returns the index tree encoded by a v2 snapshot. Legacy
// one-parent snapshots return an empty tree and use the mixed-reset fallback.
func (r *Repo) SnapshotIndexTree(snapshotCommit string) (string, error) {
	line, err := r.Run("rev-list", "--parents", "-n", "1", snapshotCommit)
	if err != nil {
		return "", err
	}
	parents := strings.Fields(line)
	if len(parents) < 3 {
		return "", nil
	}
	return r.TreeOf(parents[2])
}

// FetchBranch fetches the branch head and its snapshot ref from the hub.
func (r *Repo) FetchBranch(branch string) error {
	_, err := r.RunRemote("fetch", RemoteName,
		"+refs/heads/"+branch+":refs/remotes/"+RemoteName+"/"+branch,
		"+"+SnapshotRefPrefix+branch+":"+SnapshotRefPrefix+branch)
	return err
}

// PushBranch pushes the branch head (and optionally its snapshot ref) to the
// hub, carrying the lease push token as a push option for the pre-receive
// fencing hook.
func (r *Repo) PushBranch(branch, pushToken string, withSnapshot bool) error {
	args := []string{"push", RemoteName, "--push-option=zync-token=" + pushToken,
		"refs/heads/" + branch + ":refs/heads/" + branch}
	if withSnapshot {
		args = append(args, "+"+SnapshotRefPrefix+branch+":"+SnapshotRefPrefix+branch)
	}
	_, err := r.RunRemote(args...)
	return err
}

// Restore materializes a flushed snapshot: it leaves the repo on `branch` at
// baseCommit with the snapshot's changes present as uncommitted work.
//
// normCommit must be a commit whose tree equals the current working state
// (the caller verifies this beforehand); it lets git track every current file
// so that files deleted between the two states are removed cleanly.
func (r *Repo) Restore(branch, normCommit, snapshotCommit, baseCommit, indexTree string) error {
	// Detach so no branch ref moves during normalization.
	if _, err := r.Run("checkout", "-f", "--detach", normCommit); err != nil {
		return err
	}
	// Worktree becomes exactly the snapshot state (adds, edits, deletes).
	if _, err := r.Run("reset", "--hard", snapshotCommit); err != nil {
		return err
	}
	// Point the branch at its real head and re-attach without touching files.
	if _, err := r.Run("branch", "-f", branch, baseCommit); err != nil {
		return err
	}
	if _, err := r.Run("symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
		return err
	}
	if indexTree == "" {
		// Legacy snapshots flattened the index into unstaged changes.
		if _, err := r.Run("reset", "--mixed", baseCommit); err != nil {
			return err
		}
	} else if _, err := r.Run("read-tree", indexTree); err != nil {
		return err
	}
	return nil
}
