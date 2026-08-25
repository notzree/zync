package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotRestorePreservesIndexAndWorktree(t *testing.T) {
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-q", "-b", "main")
	runTestGit(t, dir, "config", "user.name", "zync-test")
	runTestGit(t, dir, "config", "user.email", "zync-test@localhost")
	writeTestFile(t, dir, "mixed.txt", "base\n")
	writeTestFile(t, dir, "unstaged.txt", "base\n")
	runTestGit(t, dir, "add", "-A")
	runTestGit(t, dir, "commit", "-qm", "base")

	writeTestFile(t, dir, "mixed.txt", "staged\n")
	writeTestFile(t, dir, "staged.txt", "staged addition\n")
	runTestGit(t, dir, "add", "mixed.txt", "staged.txt")
	writeTestFile(t, dir, "mixed.txt", "staged\nunstaged\n")
	writeTestFile(t, dir, "unstaged.txt", "unstaged only\n")
	writeTestFile(t, dir, "untracked.txt", "untracked\n")

	repo, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	base, err := repo.RevParse("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	beforeIndex, err := repo.IndexTree()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _, indexTree, err := repo.Snapshot("main")
	if err != nil {
		t.Fatal(err)
	}
	afterIndex, err := repo.IndexTree()
	if err != nil {
		t.Fatal(err)
	}
	if beforeIndex != indexTree || afterIndex != indexTree {
		t.Fatalf("snapshot changed or misrecorded index: before=%s saved=%s after=%s", beforeIndex, indexTree, afterIndex)
	}
	decoded, err := repo.SnapshotIndexTree(snapshot)
	if err != nil || decoded != indexTree {
		t.Fatalf("decoded index tree = %s, %v; want %s", decoded, err, indexTree)
	}

	runTestGit(t, dir, "reset", "--hard", "HEAD")
	runTestGit(t, dir, "clean", "-fd")
	if err := repo.Restore("main", base, snapshot, base, indexTree); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, dir, "mixed.txt"); got != "staged\nunstaged\n" {
		t.Fatalf("mixed worktree = %q", got)
	}
	if got := runTestGit(t, dir, "show", ":mixed.txt"); got != "staged" {
		t.Fatalf("mixed index = %q", got)
	}
	if got := runTestGit(t, dir, "diff", "--cached", "--name-only"); !containsLine(got, "mixed.txt") || !containsLine(got, "staged.txt") {
		t.Fatalf("cached diff missing staged files: %q", got)
	}
	if got := runTestGit(t, dir, "diff", "--name-only"); !containsLine(got, "mixed.txt") || !containsLine(got, "unstaged.txt") {
		t.Fatalf("worktree diff missing unstaged files: %q", got)
	}
	if got := runTestGit(t, dir, "status", "--porcelain", "--", "untracked.txt"); got != "?? untracked.txt" {
		t.Fatalf("untracked status = %q", got)
	}
	if got := runTestGit(t, dir, "rev-parse", "HEAD"); got != base {
		t.Fatalf("branch HEAD moved to %s, want %s", got, base)
	}
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=zync-test", "GIT_AUTHOR_EMAIL=zync-test@localhost",
		"GIT_COMMITTER_NAME=zync-test", "GIT_COMMITTER_EMAIL=zync-test@localhost",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func containsLine(lines, want string) bool {
	for _, line := range strings.Split(lines, "\n") {
		if line == want {
			return true
		}
	}
	return false
}
