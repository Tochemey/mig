-- +mig step: flagged_update
UPDATE users SET nickname = handle;

-- +mig step: flagged_delete
DELETE FROM sessions;

-- +mig step: a_predicate_is_left_alone
UPDATE users SET nickname = handle WHERE nickname IS NULL;

-- +mig step: a_table_this_migration_made
CREATE TABLE staging (id int, note text);

-- +mig step: emptying_it_is_fine
UPDATE staging SET note = 'x';

-- +mig step: grant_staging
GRANT SELECT ON staging TO application;

-- +mig step: fill_email
-- +mig backfill: table=users key=id batch=5000
-- +mig satisfied: sql(SELECT NOT EXISTS (SELECT 1 FROM users WHERE email IS NULL))
UPDATE users SET email = legacy_email
 WHERE id > :cursor_lo AND id <= :cursor_hi AND email IS NULL;
