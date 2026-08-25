package hub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// preReceiveHook enforces write fencing at the git layer: every push must
// carry a push option "zync-token=<token>" matching the current held lease.
// The hook forwards the pushed ref list to the hub's validate-push endpoint,
// which also checks that only the leased branch's refs are touched.
const preReceiveHook = `#!/bin/sh
set -u
token=""
i=0
count="${GIT_PUSH_OPTION_COUNT:-0}"
while [ "$i" -lt "$count" ]; do
  opt=$(eval "printf '%s' \"\${GIT_PUSH_OPTION_$i}\"")
  case "$opt" in
    zync-token=*) token="${opt#zync-token=}" ;;
  esac
  i=$((i+1))
done
ws=$(basename "$PWD" .git)
if ! cat | curl -sSf -X POST \
  "${ZYNC_INTERNAL_URL:-http://127.0.0.1:8080}/internal/validate-push?workspace=${ws}&token=${token}" \
  -H "Authorization: Bearer ${ZYNC_TOKEN}" \
  --data-binary @- >/dev/null 2>&1; then
  echo "zync: push rejected - no valid lease token for this branch (stale generation?)" >&2
  exit 1
fi
exit 0
`

// GitManager owns the bare repositories on the hub's data volume.
type GitManager struct {
	gitBin    string
	reposDir  string
	pruneAge  time.Duration
	gcBarrier sync.Mutex
}

func NewGitManager(cfg Config) *GitManager {
	pruneAge := cfg.GitGCPruneAge
	if pruneAge <= 0 {
		pruneAge = 14 * 24 * time.Hour
	}
	return &GitManager{gitBin: cfg.GitBin, reposDir: cfg.ReposDir(), pruneAge: pruneAge}
}

func (g *GitManager) RepoPath(workspace string) string {
	return filepath.Join(g.reposDir, workspace+".git")
}

// ValidWorkspaceName rejects names that could escape the repos directory or
// break URLs.
func ValidWorkspaceName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		ok := r == '-' || r == '_' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return !strings.HasPrefix(name, ".") && !strings.Contains(name, "..")
}

func (g *GitManager) EnsureBareRepo(workspace, defaultBranch string) error {
	path := g.RepoPath(workspace)
	if _, err := os.Stat(path); err == nil {
		return g.configureRepo(path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect repository: %w", err)
	}
	if err := g.run(context.Background(), "init", "--bare", "--initial-branch="+defaultBranch, path); err != nil {
		return err
	}
	if err := g.configureRepo(path); err != nil {
		return err
	}
	hookPath := filepath.Join(path, "hooks", "pre-receive")
	if err := os.WriteFile(hookPath, []byte(preReceiveHook), 0o755); err != nil {
		return fmt.Errorf("write pre-receive hook: %w", err)
	}
	return nil
}

func (g *GitManager) configureRepo(path string) error {
	steps := [][]string{
		{"-C", path, "config", "receive.advertisePushOptions", "true"},
		{"-C", path, "config", "receive.autogc", "false"},
		{"-C", path, "config", "http.receivepack", "true"},
		{"-C", path, "config", "gc.pruneExpire", gitExpiry(g.pruneAge)},
		{"-C", path, "config", "transfer.hideRefs", "refs/zync/retained/"},
	}
	for _, args := range steps {
		if err := g.run(context.Background(), args...); err != nil {
			return err
		}
	}
	return nil
}

// RetentionRoot is a commit recorded in SQLite that must remain available even
// if a newer Git push has already replaced its public branch or snapshot ref.
type RetentionRoot struct {
	LeaseID        int64
	SnapshotCommit string
	BaseCommit     string
}

// CompactAll packs every bare repository and prunes unreachable objects older
// than the configured grace period. Repositories are processed sequentially to
// keep peak memory use bounded.
func (g *GitManager) CompactAll(ctx context.Context, roots map[string][]RetentionRoot) (int, error) {
	g.gcBarrier.Lock()
	defer g.gcBarrier.Unlock()
	return g.compactAll(ctx, roots)
}

func (g *GitManager) compactAll(ctx context.Context, roots map[string][]RetentionRoot) (int, error) {
	entries, err := os.ReadDir(g.reposDir)
	if err != nil {
		return 0, fmt.Errorf("read repositories: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var errs []error
	count := 0
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() || !strings.HasSuffix(entry.Name(), ".git") {
			continue
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		workspace := strings.TrimSuffix(entry.Name(), ".git")
		path := filepath.Join(g.reposDir, entry.Name())
		count++
		if err := g.compactRepo(ctx, path, roots[workspace]); err != nil {
			errs = append(errs, fmt.Errorf("compact %s: %w", workspace, err))
		}
	}
	return count, errors.Join(errs...)
}

func (g *GitManager) compactRepo(ctx context.Context, path string, roots []RetentionRoot) error {
	if err := g.run(ctx, "-C", path, "config", "gc.pruneExpire", gitExpiry(g.pruneAge)); err != nil {
		return err
	}

	rootErr := g.syncRetentionRefs(ctx, path, roots)
	pruneArg := "--prune=" + gitExpiry(g.pruneAge)
	if rootErr != nil {
		// Packing remains useful, but pruning without complete application roots
		// could remove the snapshot SQLite still tells clients to restore.
		pruneArg = "--no-prune"
	}
	gcErr := g.run(ctx, "-c", "pack.threads=1", "-C", path, "gc", "--quiet", pruneArg)
	return errors.Join(rootErr, gcErr)
}

func (g *GitManager) syncRetentionRefs(ctx context.Context, path string, roots []RetentionRoot) error {
	expected := make(map[string]string, len(roots)*2)
	for _, root := range roots {
		prefix := "refs/zync/retained/leases/" + strconv.FormatInt(root.LeaseID, 10) + "/"
		if root.SnapshotCommit != "" {
			expected[prefix+"snapshot"] = root.SnapshotCommit
		}
		if root.BaseCommit != "" {
			expected[prefix+"base"] = root.BaseCommit
		}
	}

	refNames := make([]string, 0, len(expected))
	for ref, oid := range expected {
		if !validObjectID(oid) {
			return fmt.Errorf("invalid object ID %q for %s", oid, ref)
		}
		if err := g.run(ctx, "-C", path, "cat-file", "-e", oid+"^{commit}"); err != nil {
			return fmt.Errorf("retention root %s is unavailable: %w", ref, err)
		}
		refNames = append(refNames, ref)
	}
	sort.Strings(refNames)
	for _, ref := range refNames {
		if err := g.run(ctx, "-C", path, "update-ref", ref, expected[ref]); err != nil {
			return err
		}
	}

	out, err := g.output(ctx, "-C", path, "for-each-ref", "--format=%(refname)", "refs/zync/retained/leases/")
	if err != nil {
		return err
	}
	for _, ref := range strings.Fields(out) {
		if _, ok := expected[ref]; ok {
			continue
		}
		if err := g.run(ctx, "-C", path, "update-ref", "-d", ref); err != nil {
			return err
		}
	}
	return nil
}

func (g *GitManager) withGCBarrier(fn func() error) error {
	g.gcBarrier.Lock()
	defer g.gcBarrier.Unlock()
	return fn()
}

func (g *GitManager) run(ctx context.Context, args ...string) error {
	_, err := g.output(ctx, args...)
	return err
}

func (g *GitManager) output(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, g.gitBin, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func gitExpiry(age time.Duration) string {
	return strconv.FormatInt(int64(age/time.Second), 10) + ".seconds.ago"
}

func validObjectID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for _, c := range oid {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
