-- +mig step: add_check
ALTER TABLE orders ADD CONSTRAINT orders_ref_nn CHECK (ref IS NOT NULL) NOT VALID;

-- +mig step: validate
ALTER TABLE orders VALIDATE CONSTRAINT orders_ref_nn;

-- +mig step: the_check_cannot_skip_the_scan_before_12
ALTER TABLE orders ALTER COLUMN ref SET NOT NULL;
