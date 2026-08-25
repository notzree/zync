-- +goose Up
ALTER TABLE workspaces ADD COLUMN archived_at INTEGER;

-- +goose Down
ALTER TABLE workspaces DROP COLUMN archived_at;
