-- +mig step: flagged
CREATE INDEX idx_users_email ON users (email);

-- +mig step: concurrent_is_fine
-- +mig notx
CREATE INDEX CONCURRENTLY idx_users_name ON users (name);

-- +mig step: new_table
CREATE TABLE audit (id bigint);

-- +mig step: index_on_new_table_is_fine
CREATE INDEX idx_audit_id ON audit (id);

-- +mig step: create_and_index_in_one_step
CREATE TABLE metrics (id int);
CREATE INDEX idx_metrics_id ON metrics (id);

-- +mig step: grant_audit
GRANT SELECT ON audit TO application;

-- +mig step: grant_metrics
GRANT SELECT ON metrics TO application;
