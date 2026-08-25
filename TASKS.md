# Zync implementation log

This file is the continuity log for long-running implementation work. Update it
when a task changes state, a design decision is made, or verification uncovers a
follow-up.

## Current queue

- [x] Hub Git compaction and snapshot retention
- [x] Agent-state bundles
- [x] Lease TTL and heartbeat
- [x] Encrypted extras channel
- [x] Staged/unstaged fidelity
- [ ] Agent workspace reconciliation
- [ ] Workspace archive/delete
- [ ] OpenCode server password support
- [ ] Full test, E2E, deployment, and documentation pass

## Hub Git compaction

Status: implemented; final full-suite verification remains

Plan:

1. Add hub settings for the GC interval and unreachable-object grace period.
2. Protect lease snapshot/base object IDs from the Git push/API release crash
   window with stable internal refs before pruning.
3. Compact bare repositories sequentially with foreground, single-threaded
   `git gc`; never use `--prune=now` or aggressive GC in the scheduler.
4. Run one sweep at startup and subsequent sweeps at the configured interval.
5. Add unit tests, deployment settings, metrics-through-logs, and operator docs.

Decisions:

- Snapshot refs are mutable per branch; old snapshots do not form a history.
  Compaction targets unreachable objects, while the current snapshot remains.
- Default interval: 24 hours. A zero interval disables scheduled maintenance.
- Default prune grace: 336 hours (14 days). Values below 24 hours are rejected.
- SQLite commit IDs are application roots even though Git cannot see them. GC
  must retain them until SQLite advances to the next successfully released
  snapshot.
- Maintenance failures are logged and retried; they do not make the hub
  unhealthy.
- Git receive-time auto-GC is disabled for managed repositories so every prune
  runs behind the SQLite-aware maintenance barrier.

Implemented:

- Hub config, scheduler, sequential foreground GC, hidden lease retention refs,
  release/GC coordination, real-Git tests, Kubernetes defaults, CI test step,
  and operator documentation.

## Agent-state bundles

Status: implemented; final full-suite verification remains

Known design:

- Store exported sessions as opaque, content-addressed blobs on the hub.
- Associate a blob atomically with source generation and snapshot commit during
  release; return only that exact association on take.
- Export/upload failures retain the lease. Orphan uploads are safe and can be
  garbage-collected later.

Validated interface:

- OpenCode 1.18.22 provides `opencode export <sessionID>` and
  `opencode import <file>`. `session.idle` carries `properties.sessionID`.

Implemented:

- 64 MiB bounded export/import adapter with digest verification.
- Content-addressed, mode-0600 hub blob storage and authenticated transfer.
- Transactional snapshot/source-generation association in the lease row.
- Automatic import on take, local session tracking, TUI session resume, and
  plugin handoff/export behavior.
- Unreferenced bundle pruning, OpenCode version pinning, unit tests, and an E2E
  two-replica bundle round trip.

## Lease TTL and heartbeat

Status: implemented; final full-suite verification remains

Key requirements:

- Expiry is evaluated transactionally by hub time, not by a background reaper.
- Normal take can claim an expired lease; generation and push token rotate.
- Heartbeats renew a specific holder/generation and cannot resurrect expiry.
- Push validation renews the lease to protect the push/release window.
- Roll out schema and heartbeat-capable clients with TTL disabled before
  enabling a finite default.

Implemented:

- Nullable Unix-millisecond deadlines with a configurable two-minute default.
- Atomic heartbeat and push-time renewal, effective `expired` lease state, and
  non-force takeover with generation/token rotation.
- Safe same-holder reclaim when no generation intervened.
- Client heartbeat/watch command, OpenCode plugin timer, local expiry status,
  Kubernetes config, deterministic service tests, and E2E expiry coverage.

## Remaining work notes

- Agent cloning currently runs only at container startup and needs continuous,
  atomic reconciliation.
- Workspace archive should precede irreversible deletion.
- The agent must fail closed without `OPENCODE_SERVER_PASSWORD`, and OpenCode
  should be pinned before depending on session APIs.

## Encrypted extras

Status: implemented; final full-suite verification remains

Implemented:

- Tracked versioned allowlist with age X25519 recipients and local-only
  `ZYNC_AGE_IDENTITY_FILE` identities.
- Bounded regular-file payloads, deletion markers, path/symlink validation,
  client-side encryption/decryption, and digest verification.
- Ciphertext-only content-addressed hub transfer, transactional snapshot and
  source-generation association, and orphan pruning.
- Unit tests and E2E `.env` round trip/deletion coverage, including an assertion
  that the hub blob store contains no secret plaintext.

## Staged/unstaged fidelity

Status: implemented; final full-suite verification remains

Implemented:

- V2 snapshots encode the real index tree in a second-parent metadata commit;
  legacy one-parent snapshots retain the unstaged fallback.
- Restore loads the saved index without changing restored worktree bytes.
- Divergence checks compare both worktree and index trees.
- Git plumbing tests and E2E coverage include staged additions, mixed staged
  and unstaged edits, untracked files, and staging-only divergence.
