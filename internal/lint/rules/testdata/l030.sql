-- +mig step: flagged_column
ALTER TABLE users DROP COLUMN legacy_email;

-- +mig step: flagged_table
DROP TABLE sessions;

-- +mig step: flagged_qualified_table
DROP TABLE ops.audit_log;

-- +mig step: scaffolding_this_migration_made
CREATE TABLE staging (id int);

-- +mig step: dropping_it_again_is_fine
DROP TABLE staging;
