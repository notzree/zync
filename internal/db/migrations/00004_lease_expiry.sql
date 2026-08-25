-- +goose Up
ALTER TABLE leases ADD COLUMN expires_at INTEGER;
CREATE INDEX leases_expiry ON leases (state, expires_at);

-- +goose Down
DROP INDEX leases_expiry;
ALTER TABLE leases DROP COLUMN expires_at;
