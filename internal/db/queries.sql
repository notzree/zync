-- name: CreateWorkspace :one
INSERT INTO workspaces (name, default_branch)
VALUES (?, ?)
RETURNING *;

-- name: GetWorkspaceByName :one
SELECT * FROM workspaces WHERE name = ? AND archived_at IS NULL;

-- name: GetWorkspaceAnyByName :one
SELECT * FROM workspaces WHERE name = ?;

-- name: GetWorkspaceByID :one
SELECT * FROM workspaces WHERE id = ?;

-- name: ListWorkspaces :many
SELECT * FROM workspaces WHERE archived_at IS NULL ORDER BY name;

-- name: ListAllWorkspaces :many
SELECT * FROM workspaces ORDER BY name;

-- name: ArchiveWorkspace :one
UPDATE workspaces SET archived_at = ? WHERE id = ? RETURNING *;

-- name: RestoreWorkspace :one
UPDATE workspaces SET archived_at = NULL WHERE id = ? RETURNING *;

-- name: UpsertReplica :one
INSERT INTO replicas (name, last_seen_at, opencode_url, workspaces_dir)
VALUES (?, datetime('now'), ?, ?)
ON CONFLICT (name) DO UPDATE SET
    last_seen_at = datetime('now'),
    opencode_url = CASE WHEN excluded.opencode_url != '' THEN excluded.opencode_url ELSE replicas.opencode_url END,
    workspaces_dir = CASE WHEN excluded.workspaces_dir != '' THEN excluded.workspaces_dir ELSE replicas.workspaces_dir END
RETURNING *;

-- name: ListReplicas :many
SELECT * FROM replicas ORDER BY name;

-- name: GetLease :one
SELECT * FROM leases WHERE workspace_id = ? AND branch = ?;

-- name: CreateHeldLease :one
INSERT INTO leases (workspace_id, branch, holder_replica_id, generation, state, push_token, expires_at)
VALUES (?, ?, ?, 1, 'held', ?, ?)
RETURNING *;

-- name: GrantLease :one
UPDATE leases
SET holder_replica_id = ?,
    generation = generation + 1,
    state = 'held',
    push_token = ?,
    expires_at = ?,
    updated_at = datetime('now')
WHERE id = ?
RETURNING *;

-- name: RenewLease :one
UPDATE leases
SET expires_at = sqlc.arg(new_expires_at)
WHERE id = sqlc.arg(id)
  AND holder_replica_id = sqlc.arg(holder_replica_id)
  AND generation = sqlc.arg(generation)
  AND push_token = sqlc.arg(push_token)
  AND state = 'held'
  AND (expires_at IS NULL OR expires_at > sqlc.arg(now))
RETURNING *;

-- name: ReleaseLease :one
UPDATE leases
SET state = 'released',
    push_token = NULL,
    snapshot_commit = ?,
    base_commit = ?,
    agent_state_digest = ?,
    agent_state_size = ?,
    agent_state_format = ?,
    agent_session_id = ?,
    agent_state_generation = ?,
    extras_digest = ?,
    extras_size = ?,
    extras_format = ?,
    extras_generation = ?,
    expires_at = NULL,
    updated_at = datetime('now')
WHERE id = ?
RETURNING *;

-- name: RenewLeaseByPushToken :one
UPDATE leases
SET expires_at = sqlc.arg(new_expires_at)
WHERE push_token = sqlc.arg(push_token)
  AND state = 'held'
  AND (expires_at IS NULL OR expires_at > sqlc.arg(now))
RETURNING *;

-- name: ListAllLeases :many
SELECT l.*, w.name AS workspace_name, r.name AS holder_name,
       r.opencode_url AS holder_opencode_url, r.workspaces_dir AS holder_workspaces_dir
FROM leases l
JOIN workspaces w ON w.id = l.workspace_id
LEFT JOIN replicas r ON r.id = l.holder_replica_id
ORDER BY w.name, l.branch;

-- name: ListActiveLeases :many
SELECT l.*, w.name AS workspace_name, r.name AS holder_name,
       r.opencode_url AS holder_opencode_url, r.workspaces_dir AS holder_workspaces_dir
FROM leases l
JOIN workspaces w ON w.id = l.workspace_id
LEFT JOIN replicas r ON r.id = l.holder_replica_id
WHERE w.archived_at IS NULL
ORDER BY w.name, l.branch;

-- name: ListLeasesByWorkspace :many
SELECT l.*, w.name AS workspace_name, r.name AS holder_name,
       r.opencode_url AS holder_opencode_url, r.workspaces_dir AS holder_workspaces_dir
FROM leases l
JOIN workspaces w ON w.id = l.workspace_id
LEFT JOIN replicas r ON r.id = l.holder_replica_id
WHERE l.workspace_id = ?
ORDER BY l.branch;
