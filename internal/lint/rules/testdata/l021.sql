-- +mig step: flagged
ALTER TABLE users VALIDATE CONSTRAINT users_age_positive;
ALTER TABLE users ADD COLUMN note text;
ALTER TABLE users ADD COLUMN handle text;

-- +mig step: flagged_pair
ALTER TABLE shipments VALIDATE CONSTRAINT shipments_age_positive;
ALTER TABLE shipments ADD COLUMN note text;

-- +mig step: catalog_work_together_is_fine
ALTER TABLE orders ADD COLUMN note text;
ALTER TABLE orders ADD COLUMN handle text;

-- +mig step: a_read_alongside_is_fine
ALTER TABLE payments VALIDATE CONSTRAINT payments_age_positive;
SELECT count(*) FROM payments;

-- +mig step: long_work_alone_is_fine
ALTER TABLE invoices VALIDATE CONSTRAINT invoices_age_positive;

-- +mig step: building_this_steps_own_table_is_fine
CREATE TABLE totals (id bigint, amount bigint);
CREATE INDEX idx_totals_amount ON totals (amount);
ALTER TABLE totals ADD COLUMN note text;

-- +mig step: a_foreign_key_on_a_new_table_is_fine
CREATE TABLE receipts (id bigint, user_id bigint REFERENCES users (id));
ALTER TABLE receipts ADD COLUMN note text;
