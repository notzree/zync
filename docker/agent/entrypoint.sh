#!/usr/bin/env bash
# Boot sequence for the zync agent runtime:
#   1. install the zync opencode plugin into the volume-backed HOME
#   2. clone any hub workspaces that are not yet materialized
#   3. run the headless opencode server
set -euo pipefail

: "${ZYNC_HUB_URL:?ZYNC_HUB_URL is required}"
: "${ZYNC_TOKEN:?ZYNC_TOKEN is required}"
: "${ZYNC_REPLICA:?ZYNC_REPLICA is required}"
WORKSPACES_DIR="${ZYNC_WORKSPACES_DIR:-/workspaces}"

mkdir -p "$HOME/.config/opencode/plugins" "$WORKSPACES_DIR"
cp /opt/zync/plugins/zync.js "$HOME/.config/opencode/plugins/zync.js"

git config --global user.name "${ZYNC_REPLICA}"
git config --global user.email "${ZYNC_REPLICA}@zync.local"
git config --global --add safe.directory '*'

echo "zync-agent: syncing workspaces from ${ZYNC_HUB_URL}"
for ws in $(zx ls); do
  if [ ! -d "$WORKSPACES_DIR/$ws" ]; then
    echo "zync-agent: cloning workspace $ws"
    zx clone "$ws" "$WORKSPACES_DIR/$ws" || echo "zync-agent: clone of $ws failed; continuing"
  fi
done

echo "zync-agent: starting opencode server on :4096"
cd "$WORKSPACES_DIR"
exec opencode serve --hostname 0.0.0.0 --port 4096
