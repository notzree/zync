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
cat >"$T/opencode-test" <<'SH'
#!/bin/sh
set -eu
case "$1" in
  export) printf '{"info":{"id":"%s"},"messages":[]}\n' "$2" ;;
  import) cp "$2" "$ZYNC_E2E_IMPORT_LOG"; printf 'Imported session: ses_e2e\n' ;;
  *) exit 2 ;;
esac
SH
chmod +x "$T/opencode-test"
export ZYNC_OPENCODE_BIN="$T/opencode-test" ZYNC_E2E_IMPORT_LOG="$T/imported-session.json"
cat >"$T/age-key.txt" <<'KEY'
AGE-SECRET-KEY-186M5S24RLAW4CLALRPWGUJM9TJYLZ79X9DLH0F5SRNSW6ADPJHTSZGCK63
KEY
export ZYNC_AGE_IDENTITY_FILE="$T/age-key.txt"
ZYNC_TOKEN=testtoken ZYNC_PORT=$PORT ZYNC_DATA_DIR="$T/data" ZYNC_LEASE_TTL=3s "$T/zync-hub" >"$T/hub.log" 2>&1 &
HUB_PID=$!
for i in $(seq 1 50); do
  curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null || fail "hub did not start (see $T/hub.log)"
echo "hub is up"

export ZYNC_HUB_URL="http://127.0.0.1:$PORT" ZYNC_TOKEN=testtoken
laptop() { ZYNC_REPLICA=laptop ZYNC_CONFIG_DIR="$T/laptop-cfg" "$T/zync" "$@"; }
server() { ZYNC_REPLICA=server ZYNC_REPLICA_KIND=remote ZYNC_CONFIG_DIR="$T/server-cfg" "$T/zync" "$@"; }

# --- laptop: create repo, enroll, dirty it, hand off -------------------------
mkdir "$T/laptop" && cd "$T/laptop"
git init -q -b main
git config user.email test@test && git config user.name test
echo 'package main' > main.go
echo '.env' > .gitignore
cat >.zync-extras.json <<'JSON'
{"version":1,"paths":[".env"],"recipients":["age1neqapsr5mtv9tghnlyjz4c8mvp94hcsdzdj3s6r7zmv8mf2t2qrsnw2qu9"]}
JSON
echo 'API_TOKEN=laptop-secret' > .env
git add -A && git commit -qm "initial"
laptop init --workspace demo
echo '// wip edit from laptop' >> main.go        # unstaged edit
echo 'staged handoff state' > staged.txt
git add staged.txt
echo 'staged portion' > mixed.txt
git add mixed.txt
echo 'unstaged portion' >> mixed.txt
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
server heartbeat
grep -q 'API_TOKEN=laptop-secret' .env || fail "encrypted extra did not transfer to server"
if grep -R -a 'API_TOKEN=laptop-secret' "$T/data/blobs" >/dev/null 2>&1; then
  fail "hub blob store contains extras plaintext"
fi
grep -q 'wip edit from laptop' main.go || fail "unstaged edit did not transfer"
[ "$(cat notes.txt)" = "laptop scratch notes" ] || fail "untracked file did not transfer"
git diff --quiet && fail "edits should be UNCOMMITTED on the server"
git diff --cached --name-only | grep '^staged.txt$' >/dev/null || fail "staged addition did not remain staged"
git diff --cached --name-only | grep '^mixed.txt$' >/dev/null || fail "mixed file lost its staged portion"
git diff --name-only | grep '^mixed.txt$' >/dev/null || fail "mixed file lost its unstaged portion"
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
echo 'agent staged state' > agent-staged.txt
git add agent-staged.txt
echo 'API_TOKEN=server-secret' > .env
server handoff --to laptop --agent-session ses_e2e
server status | grep 'held' | grep 'laptop' >/dev/null || fail "directed handoff should leave laptop as holder"
echo "directed handoff granted lease straight to laptop"

# --- laptop: take (sync the already-granted lease), verify state -------------
cd "$T/laptop"
take_output="$(laptop take)"
echo "$take_output"
echo "$take_output" | grep 'agent session restored: ses_e2e' >/dev/null || fail "agent session was not reported as restored"
[ -f "$T/imported-session.json" ] || fail "agent session was not imported"
grep '"id":"ses_e2e"' "$T/imported-session.json" >/dev/null || fail "wrong agent session was imported"
git log --oneline | grep "agent work" >/dev/null || fail "server commit did not come back"
grep -q 'wip from agent' agent.go || fail "server dirty state did not come back"
grep -q 'wip edit from laptop' main.go || fail "original laptop edit lost"
grep -q 'API_TOKEN=server-secret' .env || fail "updated encrypted extra did not come back"
git status --porcelain | grep 'agent.go' >/dev/null || fail "agent wip should be uncommitted"
git diff --cached --name-only | grep '^agent-staged.txt$' >/dev/null || fail "agent staging state did not come back"
echo "laptop took lease back; commits and dirty state intact"

# --- divergence detection: edit without lease, then try to take --------------
laptop release
git add agent.go
if laptop take 2>/dev/null; then fail "staging-only divergence should block take"; fi
git reset -q agent.go
echo 'rogue edit' >> main.go
if laptop take 2>/dev/null; then fail "divergence should block take"; fi
echo "divergence correctly detected and blocked"

# --- force take adopts the diverged local state, nothing lost -----------------
laptop take --force
grep -q 'rogue edit' main.go || fail "force take should keep local edits"
rm .env
laptop release
echo "force take adopted local state and flushed it"

# --- expired holders reclaim safely; stopped heartbeats allow takeover -------
cd "$T/server-copy"
server take
[ ! -e .env ] || fail "encrypted extra deletion did not propagate"
echo '// survives expiry reclaim' >> agent.go
sleep 4
server take
grep -q 'survives expiry reclaim' agent.go || fail "same-holder expiry reclaim lost local work"
server release
server take
sleep 2
server heartbeat
sleep 2
cd "$T/laptop"
if laptop take 2>/dev/null; then fail "heartbeat should keep the lease active"; fi
sleep 2
laptop take
if server heartbeat 2>/dev/null; then fail "expired holder heartbeat should be fenced"; fi
echo "heartbeat kept the live holder active and expiry allowed non-force takeover"

# --- local (human) replica leases never expire --------------------------------
sleep 4
cd "$T/server-copy"
if server take 2>/dev/null; then fail "local replica lease must not expire without force"; fi
cd "$T/laptop"
echo "local replica lease survived past the TTL without heartbeats"

# --- agent workspace reconciliation discovers later enrollments --------------
mkdir "$T/later" && cd "$T/later"
git init -q -b main
git config user.email test@test && git config user.name test
echo 'later workspace' > README.md
git add -A && git commit -qm "initial"
laptop init --workspace later
laptop release
server sync-workspaces --root "$T/managed-workspaces"
[ -f "$T/managed-workspaces/later/.git/zync-state.json" ] || fail "new workspace was not reconciled"
server sync-workspaces --root "$T/managed-workspaces"
echo "workspace reconciliation cloned later enrollment and remained idempotent"

# --- workspace archive is reversible and excluded from reconciliation --------
laptop workspace archive later
if server ls | grep '^later$' >/dev/null; then fail "archived workspace remained active"; fi
server workspace list | grep $'later\tarchived' >/dev/null || fail "archive state was not listed"
server sync-workspaces --root "$T/active-after-archive"
[ ! -e "$T/active-after-archive/later" ] || fail "agent reconciled an archived workspace"
cd "$T/later"
if laptop take 2>/dev/null; then fail "archived workspace should reject take"; fi
laptop workspace restore later
laptop take
if laptop workspace archive later 2>/dev/null; then fail "archive should reject a live lease"; fi
laptop release
laptop workspace archive later
laptop workspace restore later
echo "workspace archive hid active use, rejected live leases, and restored cleanly"

echo
echo "ALL E2E CHECKS PASSED"
