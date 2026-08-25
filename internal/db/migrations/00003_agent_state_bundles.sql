-- +goose Up
ALTER TABLE leases ADD COLUMN agent_state_digest TEXT;
ALTER TABLE leases ADD COLUMN agent_state_size INTEGER;
ALTER TABLE leases ADD COLUMN agent_state_format TEXT;
ALTER TABLE leases ADD COLUMN agent_session_id TEXT;
ALTER TABLE leases ADD COLUMN agent_state_generation INTEGER;

-- +goose Down
ALTER TABLE leases DROP COLUMN agent_state_generation;
ALTER TABLE leases DROP COLUMN agent_session_id;
ALTER TABLE leases DROP COLUMN agent_state_format;
ALTER TABLE leases DROP COLUMN agent_state_size;
ALTER TABLE leases DROP COLUMN agent_state_digest;
