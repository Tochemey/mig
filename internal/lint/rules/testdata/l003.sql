-- +mig step: volatile_default
ALTER TABLE users ADD COLUMN token uuid NOT NULL DEFAULT gen_random_uuid();

-- +mig step: constant_default_is_fine
ALTER TABLE users ADD COLUMN score int DEFAULT 0;

-- +mig step: plain_is_fine
ALTER TABLE users ADD COLUMN notes text;

-- +mig step: new_table
CREATE TABLE audit (id bigint);

-- +mig step: volatile_on_new_table_is_fine
ALTER TABLE audit ADD COLUMN token uuid DEFAULT gen_random_uuid();

-- +mig step: sibling_rewrite_is_not_the_columns_fault
ALTER TABLE users ADD COLUMN plain int, ALTER COLUMN other TYPE bigint;

-- +mig step: serial_has_no_simple_fix
ALTER TABLE users ADD COLUMN seq bigserial;

-- +mig step: identity_has_no_simple_fix
ALTER TABLE users ADD COLUMN n bigint GENERATED ALWAYS AS IDENTITY;
