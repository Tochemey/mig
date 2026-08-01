-- +mig step: flagged
-- +mig notx
CREATE INDEX CONCURRENTLY idx_users_email ON users (email);

-- +mig step: fill_email
-- +mig backfill: table=users key=id batch=5000
-- +mig satisfied: sql(SELECT NOT EXISTS (SELECT 1 FROM users WHERE email IS NULL))
UPDATE users SET email = legacy_email
 WHERE id > :cursor_lo AND id <= :cursor_hi AND email IS NULL;

-- +mig step: index_after_the_backfill_is_fine
-- +mig notx
CREATE INDEX CONCURRENTLY idx_users_legacy ON users (legacy_email);

-- +mig step: another_table_is_fine
-- +mig notx
CREATE INDEX CONCURRENTLY idx_orders_note ON orders (note);

-- +mig step: flagged_qualified
-- +mig notx
CREATE INDEX CONCURRENTLY idx_audit_note ON app.audit (note);

-- +mig step: fill_audit_note
-- +mig backfill: table=app.audit key=id batch=5000
-- +mig satisfied: sql(SELECT NOT EXISTS (SELECT 1 FROM app.audit WHERE note IS NULL))
UPDATE app.audit SET note = legacy_note
 WHERE id > :cursor_lo AND id <= :cursor_hi AND note IS NULL;
