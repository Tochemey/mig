-- +mig step: concurrently_in_tx
CREATE INDEX CONCURRENTLY idx_users_email ON users (email);

-- +mig step: vacuum_in_tx
VACUUM users;

-- +mig step: notx_is_fine
-- +mig notx
CREATE INDEX CONCURRENTLY idx_users_name ON users (name);
