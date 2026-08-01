-- +mig step: flagged
ALTER TABLE orders ADD CONSTRAINT orders_user_fk FOREIGN KEY (user_id) REFERENCES users (id) NOT VALID;

-- +mig step: a_check_before_the_backfill_is_fine
ALTER TABLE orders ADD CONSTRAINT orders_note_present CHECK (note IS NOT NULL) NOT VALID;

-- +mig step: fill_user_id
-- +mig backfill: table=orders key=id batch=5000
-- +mig satisfied: sql(SELECT NOT EXISTS (SELECT 1 FROM orders WHERE user_id IS NULL))
UPDATE orders SET user_id = legacy_user
 WHERE id > :cursor_lo AND id <= :cursor_hi AND user_id IS NULL;

-- +mig step: key_after_the_backfill_is_fine
ALTER TABLE orders ADD CONSTRAINT orders_owner_fk FOREIGN KEY (owner_id) REFERENCES users (id) NOT VALID;
