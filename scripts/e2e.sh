#!/usr/bin/env bash
# End-to-end smoke test: runs a real hub, then simulates a laptop and a
# server replica handing a workspace back and forth with dirty state.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
T="$(mktemp -d /tmp/zync-e2e.XXXXXX)"
PORT=8377
HUB_PID=""

cleanup() {
  [ -n "$HUB_PID" ] && kill "$HUB_PID" 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT

fail() { echo "FAIL: $1" >&2; exit 1; }

cd "$ROOT"
go build -o "$T/zync" ./cmd/zync
go build -o "$T/zync-hub" ./cmd/zync-hub

mkdir -p "$T/data" "$T/laptop-cfg" "$T/server-cfg"
ZYNC_TOKEN=testtoken ZYNC_PORT=$PORT ZYNC_DATA_DIR="$T/data" "$T/zync-hub" >"$T/hub.log" 2>&1 &
HUB_PID=$!
for i in $(seq 1 50); do
  curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null || fail "hub did not start (see $T/hub.log)"
echo "hub is up"

export ZYNC_HUB_URL="http://127.0.0.1:$PORT" ZYNC_TOKEN=testtoken
laptop() { ZYNC_REPLICA=laptop ZYNC_CONFIG_DIR="$T/laptop-cfg" "$T/zync" "$@"; }
server() { ZYNC_REPLICA=server ZYNC_CONFIG_DIR="$T/server-cfg" "$T/zync" "$@"; }

# --- laptop: create repo, enroll, dirty it, hand off -------------------------
mkdir "$T/laptop" && cd "$T/laptop"
git init -q -b main
git config user.email test@test && git config user.name test
echo 'package main' > main.go
git add -A && git commit -qm "initial"
laptop init --workspace demo
echo '// wip edit from laptop' >> main.go        # unstaged edit
echo 'laptop scratch notes' > notes.txt          # untracked file
laptop release
echo "laptop released"

# --- unauthorized push must be fenced out ------------------------------------
if git -c http.extraHeader="Authorization: Bearer testtoken" push -q \
    "http://127.0.0.1:$PORT/git/demo.git" main:refs/heads/evil 2>/dev/null; then
  fail "push without lease token should have been rejected"
fi
echo "fencing hook rejected tokenless push"

# --- server: clone, take, verify dirty state arrived intact ------------------
cd "$T"
server clone demo server-copy
cd "$T/server-copy"
git config user.email test@test && git config user.name test
server take
grep -q 'wip edit from laptop' main.go || fail "unstaged edit did not transfer"
[ "$(cat notes.txt)" = "laptop scratch notes" ] || fail "untracked file did not transfer"
git diff --quiet && fail "edits should be UNCOMMITTED on the server"
git log --oneline | grep -i snapshot >/dev/null && fail "snapshot commits must not pollute branch history"
echo "server took lease; dirty state restored as uncommitted work"

# --- laptop must not be able to take while server holds ----------------------
cd "$T/laptop"
if laptop take 2>/dev/null; then fail "take should conflict while server holds the lease"; fi
echo "concurrent take correctly refused"

# --- server: do agent-style work, then DIRECTED handoff back to laptop -------
cd "$T/server-copy"
echo 'package agent' > agent.go
git add -A && git commit -qm "agent work"
echo '// wip from agent' >> agent.go
server handoff --to laptop
server status | grep 'held' | grep 'laptop' >/dev/null || fail "directed handoff should leave laptop as holder"
echo "directed handoff granted lease straight to laptop"

# --- laptop: take (sync the already-granted lease), verify state -------------
cd "$T/laptop"
laptop take
git log --oneline | grep "agent work" >/dev/null || fail "server commit did not come back"
grep -q 'wip from agent' agent.go || fail "server dirty state did not come back"
grep -q 'wip edit from laptop' main.go || fail "original laptop edit lost"
git status --porcelain | grep 'agent.go' >/dev/null || fail "agent wip should be uncommitted"
echo "laptop took lease back; commits and dirty state intact"

# --- divergence detection: edit without lease, then try to take --------------
laptop release
echo 'rogue edit' >> main.go
if laptop take 2>/dev/null; then fail "divergence should block take"; fi
echo "divergence correctly detected and blocked"

# --- force take adopts the diverged local state, nothing lost -----------------
laptop take --force
grep -q 'rogue edit' main.go || fail "force take should keep local edits"
laptop release
echo "force take adopted local state and flushed it"

echo
echo "ALL E2E CHECKS PASSED"
