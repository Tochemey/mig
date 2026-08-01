-- +mig step: flagged
-- +mig no_lock_timeout
ALTER TABLE users ADD COLUMN nickname text;

-- +mig step: with_the_timeout_is_fine
ALTER TABLE users ADD COLUMN handle text;

-- +mig step: weak_locks_without_it_are_fine
-- +mig no_lock_timeout
SELECT count(*) FROM users;
