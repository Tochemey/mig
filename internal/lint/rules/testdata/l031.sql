-- +mig step: flagged_column
ALTER TABLE users RENAME COLUMN handle TO nickname;

-- +mig step: flagged_table
ALTER TABLE sessions RENAME TO user_sessions;

-- +mig step: renaming_an_index_is_fine
ALTER INDEX idx_users_email RENAME TO idx_users_address;

-- +mig step: a_table_this_migration_made
CREATE TABLE staging (id int);

-- +mig step: renaming_it_is_fine
ALTER TABLE staging RENAME TO staging_v2;

-- +mig step: grant_staging
GRANT SELECT ON staging TO application;
