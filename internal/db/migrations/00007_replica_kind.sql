-- +goose Up
-- 'local' replicas (human machines) hold leases without expiry; 'remote'
-- replicas (unattended agent runtimes) get TTL leases and must heartbeat.
ALTER TABLE replicas ADD COLUMN kind TEXT NOT NULL DEFAULT 'local';

-- +goose Down
ALTER TABLE replicas DROP COLUMN kind;
