package hub

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompactAllRetainsDatabaseCommitsAndPrunesOldObjects(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	dataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataDir, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := NewGitManager(Config{DataDir: dataDir, GitBin: gitBin, GitGCPruneAge: 24 * time.Hour})
	if err := g.EnsureBareRepo("demo", "main"); err != nil {
		t.Fatal(err)
	}
	repo := g.RepoPath("demo")

	tree := runGit(t, repo, "mktree")
	base := runGit(t, repo, "commit-tree", tree, "-m", "base")
	runGit(t, repo, "update-ref", "refs/heads/main", base)
	snapshot := runGit(t, repo, "commit-tree", tree, "-p", base, "-m", "snapshot")
	current := runGit(t, repo, "commit-tree", tree, "-p", base, "-m", "current snapshot")
	runGit(t, repo, "update-ref", "refs/zync/snapshots/main", current)
	runGit(t, repo, "update-ref", "refs/zync/retained/leases/99/snapshot", snapshot)

	dangling := runGitInput(t, repo, []byte("discard me\n"), "hash-object", "-w", "--stdin")
	objectPath := filepath.Join(repo, "objects", dangling[:2], dangling[2:])
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(objectPath, old, old); err != nil {
		t.Fatal(err)
	}

	count, err := g.CompactAll(context.Background(), map[string][]RetentionRoot{
		"demo": {{LeaseID: 1, SnapshotCommit: snapshot, BaseCommit: base}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("compacted %d repositories, want 1", count)
	}
	if got := runGit(t, repo, "config", "--get", "receive.autogc"); got != "false" {
		t.Fatalf("receive.autogc = %q, want false", got)
	}
	if got := runGit(t, repo, "rev-parse", "refs/zync/retained/leases/1/snapshot"); got != snapshot {
		t.Fatalf("retained snapshot = %s, want %s", got, snapshot)
	}
	if refExists(repo, "refs/zync/retained/leases/99/snapshot") {
		t.Fatal("stale retention ref still exists")
	}
	if !objectExists(repo, snapshot) {
		t.Fatal("database snapshot was pruned")
	}
	if objectExists(repo, dangling) {
		t.Fatal("old unreachable object was not pruned")
	}
}

func TestCompactAllDoesNotPruneWithInvalidDatabaseRoot(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	dataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataDir, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := NewGitManager(Config{DataDir: dataDir, GitBin: gitBin, GitGCPruneAge: 24 * time.Hour})
	if err := g.EnsureBareRepo("demo", "main"); err != nil {
		t.Fatal(err)
	}
	repo := g.RepoPath("demo")
	dangling := runGitInput(t, repo, []byte("keep me\n"), "hash-object", "-w", "--stdin")
	objectPath := filepath.Join(repo, "objects", dangling[:2], dangling[2:])
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(objectPath, old, old); err != nil {
		t.Fatal(err)
	}

	_, err = g.CompactAll(context.Background(), map[string][]RetentionRoot{
		"demo": {{LeaseID: 1, SnapshotCommit: "not-an-object-id"}},
	})
	if err == nil {
		t.Fatal("CompactAll succeeded with an invalid retention root")
	}
	if !objectExists(repo, dangling) {
		t.Fatal("unreachable object was pruned despite an invalid database root")
	}
}

func runGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	return runGitInput(t, repo, nil, args...)
}

func runGitInput(t *testing.T, repo string, input []byte, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-C", repo}, args...)
	cmd := exec.Command("git", cmdArgs...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=zync-test",
		"GIT_AUTHOR_EMAIL=zync-test@localhost",
		"GIT_COMMITTER_NAME=zync-test",
		"GIT_COMMITTER_EMAIL=zync-test@localhost",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(cmdArgs, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func refExists(repo, ref string) bool {
	cmd := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", ref)
	return cmd.Run() == nil
}

func objectExists(repo, oid string) bool {
	cmd := exec.Command("git", "-C", repo, "cat-file", "-e", oid)
	return cmd.Run() == nil
}
