-- +mig step: flagged
-- +mig notx
-- +mig satisfied: sql(SELECT to_regclass('idx_users_email') IS NOT NULL)
CREATE INDEX CONCURRENTLY idx_users_email ON users (email);

-- +mig step: a_predicate_that_knows_is_fine
-- +mig notx
-- +mig satisfied: sql(SELECT coalesce((SELECT indisvalid AND indisready FROM pg_index WHERE indexrelid = to_regclass('idx_users_handle')), false))
CREATE INDEX CONCURRENTLY idx_users_handle ON users (handle);

-- +mig step: inferring_it_is_fine
-- +mig notx
CREATE INDEX CONCURRENTLY idx_users_bio ON users (bio);

-- +mig step: a_plain_build_is_another_rules_business
-- +mig satisfied: sql(SELECT to_regclass('idx_users_notes') IS NOT NULL)
CREATE INDEX idx_users_notes ON users (notes);
