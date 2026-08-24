# zync

Git-based codebase handoffs between your machines. zync keeps a *shadow
repository* of your projects on an always-on hub (e.g. k3s on a homeserver)
and arbitrates a **per-branch write lease**: exactly one replica (your laptop,
an agent pod, another machine) may mutate a branch at a time. Handing off
flushes your *entire* working state — staged, unstaged, and untracked files —
and the taker restores it exactly as you left it, still uncommitted.

The point: agents can keep working on your homeserver while your laptop is
closed, and you can pull the work back to your editor mid-flight, with git
conflicts made structurally impossible in normal operation.

## How it works

- **Hub** (`zync-hub`): one container with bare git repos on a volume and a
  lease table in sqlite. Serves git smart HTTP and a small JSON API.
- **Lease** = mutex per (workspace, branch). Granted only to a replica that is
  fully synced, so every sync is a fast-forward.
- **Handoff** = two-phase: (1) *flush* — snapshot the working tree to
  `refs/zync/snapshots/<branch>` and push; (2) *release*. If the push fails,
  the lease is retained.
- **Fencing**: every grant bumps a generation and issues a push token; a
  pre-receive hook on the hub rejects any push without the current token, so a
  stale holder (laptop that slept through a `--force` take) physically cannot
  write.
- **Divergence detection**: editing without the lease is detected by tree-hash
  comparison and blocks operations until reconciled. Nothing is ever deleted.

Your existing `origin` remote is untouched; zync uses its own `zync` remote.

## Hub deployment (k3s)

```sh
kubectl create namespace zync
kubectl -n zync create secret generic zync-hub --from-literal=token="$(openssl rand -hex 24)"
kubectl apply -f k8s/zync-hub.yaml
```

The image is published to `ghcr.io/notzree/zync-hub` by the GitHub Actions
workflow on every push to main. If the package is private, add an
imagePullSecret or make the package public in GitHub package settings.

## CLI

```sh
go install github.com/notzree/zync/cmd/zync@latest

zync setup --hub http://zync.homelab:8080 --token <token> --name laptop

cd ~/code/myproject
zync init                 # enroll repo; you hold the lease on current branch
zync handoff              # flush everything + release (mid-edit is fine)
zync take [branch]        # acquire lease + sync to latest state
zync take --force         # break someone else's lease (they get fenced out)
zync status               # all leases + local state
zync tui                  # live dashboard: t take, T force-take, h handoff
zync clone <workspace>    # materialize a workspace on another machine
```

On the server/agent side, configure with env vars instead of `setup`:
`ZYNC_HUB_URL`, `ZYNC_TOKEN`, `ZYNC_REPLICA`.

Agent-harness integration is just the CLI: call `zync take` when a session
goes active on a machine and `zync handoff` when it goes background.

## Development

```sh
go build ./...
./scripts/e2e.sh   # full two-replica handoff cycle against a real hub
```

Migrations live in `internal/db/migrations` (goose); queries in
`internal/db/queries.sql` (`sqlc generate` after changes).

## Current limitations (v1)

- Gitignored files are never synced (each replica provisions its own
  `node_modules`, `.env`, ...). An encrypted side channel for allowlisted
  secrets is planned.
- Staged vs unstaged distinction is not preserved across a handoff (everything
  arrives as unstaged changes).
- Taking a branch with no flushed state requires checking it out manually
  first.
- Snapshot refs are kept forever (delta-compressed, but `git gc` policy is on
  the roadmap).
