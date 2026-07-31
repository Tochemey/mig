-- +mig step: unprotected
ALTER TABLE users ALTER COLUMN email SET NOT NULL;

-- +mig step: add_check
ALTER TABLE orders ADD CONSTRAINT orders_ref_nn CHECK (ref IS NOT NULL) NOT VALID;

-- +mig step: validate
ALTER TABLE orders VALIDATE CONSTRAINT orders_ref_nn;

-- +mig step: proven_is_fine
ALTER TABLE orders ALTER COLUMN ref SET NOT NULL;

-- +mig step: wrong_table_check
ALTER TABLE invoices ADD CONSTRAINT invoices_ref_nn CHECK (ref IS NOT NULL) NOT VALID;

-- +mig step: validate_wrong_table
ALTER TABLE invoices VALIDATE CONSTRAINT invoices_ref_nn;

-- +mig step: proof_on_another_table_does_not_count
ALTER TABLE payments ALTER COLUMN ref SET NOT NULL;

-- +mig step: new_table
CREATE TABLE audit (id int);

-- +mig step: not_null_on_new_table_is_fine
ALTER TABLE audit ALTER COLUMN id SET NOT NULL;

-- +mig step: near_miss_checks
ALTER TABLE payments ADD CONSTRAINT payments_pos CHECK (ref > 0) NOT VALID;

-- +mig step: near_miss_wrong_column
ALTER TABLE payments ADD CONSTRAINT payments_other_nn CHECK (other_ref IS NOT NULL) NOT VALID;

-- +mig step: near_miss_not_a_column
ALTER TABLE payments ADD CONSTRAINT payments_const_nn CHECK ((1) IS NOT NULL) NOT VALID;

-- +mig step: combined
ALTER TABLE users ADD COLUMN flag boolean, ALTER COLUMN flag2 SET NOT NULL;

-- +mig step: unrelated_constraint_kind
ALTER TABLE payments ADD CONSTRAINT payments_uq UNIQUE (ref);
