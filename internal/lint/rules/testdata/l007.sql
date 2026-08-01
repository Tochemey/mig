-- +mig step: flagged
ALTER TABLE users ADD CONSTRAINT users_score_positive CHECK (score > 0);

-- +mig step: not_valid_is_fine
ALTER TABLE users ADD CONSTRAINT users_age_positive CHECK (age > 0) NOT VALID;

-- +mig step: new_table
CREATE TABLE audit (id int);

-- +mig step: check_on_new_table_is_fine
ALTER TABLE audit ADD CONSTRAINT audit_id_positive CHECK (id > 0);

-- +mig step: grant_audit
GRANT SELECT ON audit TO application;
