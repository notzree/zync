-- name: CreateWorkspace :one
INSERT INTO workspaces (name, default_branch)
VALUES (?, ?)
RETURNING *;

-- name: GetWorkspaceByName :one
SELECT * FROM workspaces WHERE name = ?;

-- name: ListWorkspaces :many
SELECT * FROM workspaces ORDER BY name;

-- name: UpsertReplica :one
INSERT INTO replicas (name, last_seen_at)
VALUES (?, datetime('now'))
ON CONFLICT (name) DO UPDATE SET last_seen_at = datetime('now')
RETURNING *;

-- name: GetLease :one
SELECT * FROM leases WHERE workspace_id = ? AND branch = ?;

-- name: CreateHeldLease :one
INSERT INTO leases (workspace_id, branch, holder_replica_id, generation, state, push_token)
VALUES (?, ?, ?, 1, 'held', ?)
RETURNING *;

-- name: GrantLease :one
UPDATE leases
SET holder_replica_id = ?,
    generation = generation + 1,
    state = 'held',
    push_token = ?,
    updated_at = datetime('now')
WHERE id = ?
RETURNING *;

-- name: ReleaseLease :one
UPDATE leases
SET state = 'released',
    push_token = NULL,
    snapshot_commit = ?,
    base_commit = ?,
    updated_at = datetime('now')
WHERE id = ?
RETURNING *;

-- name: GetHeldLeaseByPushToken :one
SELECT l.*, w.name AS workspace_name
FROM leases l
JOIN workspaces w ON w.id = l.workspace_id
WHERE l.push_token = ? AND l.state = 'held';

-- name: ListAllLeases :many
SELECT l.*, w.name AS workspace_name, r.name AS holder_name
FROM leases l
JOIN workspaces w ON w.id = l.workspace_id
LEFT JOIN replicas r ON r.id = l.holder_replica_id
ORDER BY w.name, l.branch;

-- name: ListLeasesByWorkspace :many
SELECT l.*, w.name AS workspace_name, r.name AS holder_name
FROM leases l
JOIN workspaces w ON w.id = l.workspace_id
LEFT JOIN replicas r ON r.id = l.holder_replica_id
WHERE l.workspace_id = ?
ORDER BY l.branch;
