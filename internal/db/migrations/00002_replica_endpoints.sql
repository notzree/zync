-- +goose Up
-- Replicas advertise how to reach their opencode server (if they run one),
-- so clients can attach to whichever machine holds a lease.
ALTER TABLE replicas ADD COLUMN opencode_url TEXT NOT NULL DEFAULT '';
ALTER TABLE replicas ADD COLUMN workspaces_dir TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE replicas DROP COLUMN workspaces_dir;
ALTER TABLE replicas DROP COLUMN opencode_url;
