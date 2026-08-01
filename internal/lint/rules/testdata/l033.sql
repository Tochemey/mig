-- +mig step: flagged
TRUNCATE sessions;

-- +mig step: a_table_this_migration_made
CREATE TABLE staging (id int);

-- +mig step: truncating_it_is_fine
TRUNCATE staging;

-- +mig step: grant_staging
GRANT SELECT ON staging TO application;
