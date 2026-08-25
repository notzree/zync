-- +goose Up
ALTER TABLE leases ADD COLUMN extras_digest TEXT;
ALTER TABLE leases ADD COLUMN extras_size INTEGER;
ALTER TABLE leases ADD COLUMN extras_format TEXT;
ALTER TABLE leases ADD COLUMN extras_generation INTEGER;

-- +goose Down
ALTER TABLE leases DROP COLUMN extras_generation;
ALTER TABLE leases DROP COLUMN extras_format;
ALTER TABLE leases DROP COLUMN extras_size;
ALTER TABLE leases DROP COLUMN extras_digest;
