#!/usr/bin/env bash
# Boot sequence for the zync agent runtime:
#   1. install the zync opencode plugin into the volume-backed HOME
#   2. clone any hub workspaces that are not yet materialized
#   3. run the headless opencode server
set -euo pipefail

: "${ZYNC_HUB_URL:?ZYNC_HUB_URL is required}"
: "${ZYNC_TOKEN:?ZYNC_TOKEN is required}"
: "${ZYNC_REPLICA:?ZYNC_REPLICA is required}"
: "${ZYNC_OPENCODE_URL:?ZYNC_OPENCODE_URL is required}"
if [ -n "${OPENCODE_SERVER_PASSWORD_FILE:-}" ]; then
  OPENCODE_SERVER_PASSWORD="$(<"$OPENCODE_SERVER_PASSWORD_FILE")"
  export OPENCODE_SERVER_PASSWORD
fi
: "${OPENCODE_SERVER_PASSWORD:?OPENCODE_SERVER_PASSWORD or OPENCODE_SERVER_PASSWORD_FILE is required}"
WORKSPACES_DIR="${ZYNC_WORKSPACES_DIR:-/workspaces}"
export ZYNC_WORKSPACES_DIR="$WORKSPACES_DIR"

mkdir -p "$HOME/.config/opencode/plugins" "$WORKSPACES_DIR"
cp /opt/zync/plugins/zync.js "$HOME/.config/opencode/plugins/zync.js"

git config --global user.name "${ZYNC_REPLICA}"
git config --global user.email "${ZYNC_REPLICA}@zync.local"
git config --global --add safe.directory "$WORKSPACES_DIR/*"

# The hub may be restarting alongside us (e.g. a coordinated rollout); wait
# for it so the boot-time registration and workspace sync don't no-op.
echo "zync-agent: waiting for hub at ${ZYNC_HUB_URL}"
for i in $(seq 1 60); do
  curl -sf "${ZYNC_HUB_URL}/healthz" >/dev/null 2>&1 && break
  sleep 2
done

sync_workspaces() {
  echo "zync-agent: syncing workspaces from ${ZYNC_HUB_URL}"
  zx sync-workspaces --root "$WORKSPACES_DIR" || echo "zync-agent: workspace sync failed; continuing"
}

sync_workspaces
(
  while sleep "${ZYNC_WORKSPACE_SYNC_INTERVAL:-60}"; do
    sync_workspaces
  done
) &

echo "zync-agent: starting opencode server on :4096"
cd "$WORKSPACES_DIR"
exec opencode serve --hostname 0.0.0.0 --port 4096
