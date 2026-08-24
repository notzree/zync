package hub

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	gitBin   string
	reposDir string
}

func NewGitManager(cfg Config) *GitManager {
	return &GitManager{gitBin: cfg.GitBin, reposDir: cfg.ReposDir()}
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
		return nil
	}
	steps := [][]string{
		{"init", "--bare", "--initial-branch=" + defaultBranch, path},
		{"-C", path, "config", "receive.advertisePushOptions", "true"},
		{"-C", path, "config", "http.receivepack", "true"},
	}
	for _, args := range steps {
		if out, err := exec.Command(g.gitBin, args...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
		}
	}
	hookPath := filepath.Join(path, "hooks", "pre-receive")
	if err := os.WriteFile(hookPath, []byte(preReceiveHook), 0o755); err != nil {
		return fmt.Errorf("write pre-receive hook: %w", err)
	}
	return nil
}
