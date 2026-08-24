-- +goose Up
CREATE TABLE workspaces (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    default_branch TEXT NOT NULL DEFAULT 'main',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE replicas (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    last_seen_at TEXT
);

-- One row per (workspace, branch). The row is the mutex: `state` is either
-- 'held' or 'released', and `generation` is the fencing token that increments
-- on every grant. `push_token` is only valid while held and is what the git
-- pre-receive hook validates.
CREATE TABLE leases (
    id INTEGER PRIMARY KEY,
    workspace_id INTEGER NOT NULL REFERENCES workspaces(id),
    branch TEXT NOT NULL,
    holder_replica_id INTEGER REFERENCES replicas(id),
    generation INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'released',
    snapshot_commit TEXT,
    base_commit TEXT,
    push_token TEXT,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (workspace_id, branch)
);

-- +goose Down
DROP TABLE leases;
DROP TABLE replicas;
DROP TABLE workspaces;
