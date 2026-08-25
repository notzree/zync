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
- **Agent state** = an OpenCode session export can be uploaded as an opaque,
  content-addressed blob and associated transactionally with the exact
  snapshot and source lease generation.
- **Encrypted extras** = explicitly allowlisted ignored files are age-encrypted
  on the source replica; the hub stores ciphertext and never receives an
  identity or plaintext.
- **Index fidelity** = snapshots carry separate worktree and Git index trees,
  preserving staged, unstaged, mixed (`MM`), and untracked state.
- **Fencing**: every grant bumps a generation and issues a push token; a
  pre-receive hook on the hub rejects any push without the current token, so a
  stale holder (laptop that slept through a `--force` take) physically cannot
  write.
- **Lease liveness**: held leases expire after two minutes unless renewed.
  Heartbeats and accepted pushes extend the deadline; an expired lease can be
  taken normally without `--force`.
- **Divergence detection**: editing without the lease is detected by tree-hash
  comparison and blocks operations until reconciled. Live branch and snapshot
  state is never deleted by maintenance.

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
go install github.com/notzree/zync/cmd/zx@latest   # short alias, same CLI

zync setup --hub http://zync.homelab:8080 --token <token> --name laptop

cd ~/code/myproject
zync init                 # enroll repo; you hold the lease on current branch
zync release              # flush everything + release lease back to the hub
zync handoff [--to X]     # flush + transfer the lease to replica X directly
zync take [branch]        # acquire lease + sync to latest state
zync take --force         # break someone else's lease (they get fenced out)
zync heartbeat --watch    # keep a manually-held lease alive
zync status               # all leases + local state
zync tui                  # dashboard: t take, h handoff, u release, o open
zync clone <workspace>    # materialize a workspace on another machine
```

On the server/agent side, configure with env vars instead of `setup`:
`ZYNC_HUB_URL`, `ZYNC_TOKEN`, `ZYNC_REPLICA`, and `ZYNC_REPLICA_KIND=remote`.

Replica kind decides lease expiry: `local` replicas (human machines, the
default) hold leases indefinitely; `remote` replicas (unattended agents) get
TTL leases and must heartbeat, so a crashed agent self-heals instead of
wedging the workspace.

Agent-harness integration is just the CLI: call `zync take` when a session
goes active on a machine and `zync handoff` when it goes background.

## opencode integration

`integrations/opencode/zync.js` is an opencode plugin over that CLI surface:

```sh
mkdir -p ~/.config/opencode/plugins
ln -s "$(pwd)/integrations/opencode/zync.js" ~/.config/opencode/plugins/zync.js
```

Before any mutating tool call it ensures the lease is held (taking it
automatically if free, blocking the edit with the holder's name if not), and
with `ZYNC_AUTO_HANDOFF=1` set (recommended for server-side agents only) it
flushes and releases on `session.idle`. Repos not enrolled in zync are
ignored. The same plugin runs on every replica; the laptop/server transition
falls out of the lease protocol.

On `session.idle`, auto-handoff exports the current OpenCode session and sends
it with the repository snapshot. The next `zync take` imports that session into
the target OpenCode database and reports its ID. `zync tui` opens the imported
session directly; when a newly-created OpenCode session triggers the take, the
plugin blocks mutation and tells you which imported session to resume.

Manual integrations can use `zync release --agent-session <session-id>` or
`zync handoff --agent-session <session-id>`. Zync uses OpenCode's native JSON
export/import format and supports bundles up to 64 MiB. The agent image pins the
validated OpenCode version rather than installing an unbounded latest release.

## Development

```sh
go build ./...
./scripts/e2e.sh   # full two-replica handoff cycle against a real hub
```

Migrations live in `internal/db/migrations` (goose); queries in
`internal/db/queries.sql` (`sqlc generate` after changes).

## Hub maintenance

The hub runs a Git maintenance sweep at startup and every 24 hours. It packs
objects sequentially with one pack thread and prunes unreachable objects after
a 14-day grace period. The latest snapshot for each branch remains reachable;
superseded snapshots are reclaimed after the grace period. Commit IDs still
recorded in SQLite are protected by hidden internal refs before pruning.
Unreferenced agent-state blobs are reclaimed on the same schedule.

Configure the policy with:

- `ZYNC_GIT_GC_INTERVAL` (default `24h`; `0s` disables scheduled maintenance)
- `ZYNC_GIT_GC_PRUNE_AGE` (default `336h`; minimum `24h`)

Managed repositories disable Git's independent receive-time auto-GC so pruning
always runs through Zync's SQLite-aware maintenance barrier. Avoid running
manual `git gc --prune=now` against a live hub repository.

## Lease expiry

`ZYNC_LEASE_TTL` controls hub lease expiry (default `2m`; `0s` disables it).
The OpenCode plugin starts a heartbeat while a session is active and stops it
after a successful idle handoff. Accepted Git pushes also renew the deadline,
covering the push/release window. For editing outside OpenCode, run
`zync heartbeat --watch` alongside your editor.

At expiry, the old push token stops working immediately. Any replica can then
take without `--force`, which increments the generation and fences the old
holder. If the same replica returns before anyone else takes, Zync reclaims the
next generation while preserving its unflushed local work. If another holder
intervened, Zync fails closed and requires explicit reconciliation.

## Encrypted extras

Add a tracked `.zync-extras.json` manifest to a workspace:

```json
{
  "version": 1,
  "paths": [".env", "config/local.env"],
  "recipients": ["age1example..."]
}
```

Generate an X25519 identity with `age-keygen`, provision the private identity
on each authorized replica, and set:

```sh
export ZYNC_AGE_IDENTITY_FILE="$HOME/.config/zync/workspace-age-key.txt"
```

Only exact relative regular-file paths are supported. Absolute paths, `..`,
`.git`, symlinks, duplicate entries, oversized payloads, and non-X25519
recipients are rejected. The channel is capped at 128 files, 10 MiB plaintext,
and 16 MiB ciphertext. Missing allowlisted files are encoded as deletion
markers. Keep a removed path in the manifest for one successful handoff so its
deletion reaches existing replicas before removing the manifest entry itself.

Private identities never enter Git, Zync config, the hub API, or the blob
store. Recipient removal affects future bundles only; it cannot revoke access
to ciphertext a former recipient already obtained.

## Current limitations (v1)

- Gitignored files are excluded unless explicitly selected through the
  encrypted extras manifest. Large generated trees such as `node_modules`
  should still be provisioned independently on each replica.
- Unmerged/conflicted Git indexes cannot be handed off; resolve conflicts or
  save them on a rescue branch first.
- Taking a branch with no flushed state requires checking it out manually
  first.
- The latest snapshot ref for each branch is retained until the branch or
  workspace is archived; superseded snapshot objects are compacted and pruned.
