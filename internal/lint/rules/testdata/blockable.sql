-- +mig step: fresh_creations_are_not_windows
CREATE TABLE ledger (id bigint);
CREATE INDEX idx_ledger_id ON ledger (id);
CREATE TABLE journal (id bigint);

-- +mig step: ghost_cleanup_is_not_a_window
DROP INDEX IF EXISTS name_1;
DROP INDEX IF EXISTS name_2;

-- +mig step: flagged_recreation
ALTER TABLE users ADD COLUMN note text;
DROP TABLE counters;
CREATE TABLE counters (id bigint);

-- +mig step: a_temp_table_is_not_a_window
CREATE TEMPORARY TABLE scratch (id bigint);
ALTER TABLE orders ADD COLUMN note text;
DROP TABLE scratch;
