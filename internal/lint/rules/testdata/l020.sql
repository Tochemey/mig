-- +mig step: flagged
ALTER TABLE users ADD COLUMN nickname text;
ALTER TABLE orders ADD COLUMN note text;

-- +mig step: one_table_is_fine
ALTER TABLE users ADD COLUMN handle text;
ALTER TABLE users ADD COLUMN bio text;

-- +mig step: one_statement_is_fine
ALTER TABLE invoices ADD COLUMN note text;

-- +mig step: weaker_locks_are_fine
ALTER INDEX idx_users_email RENAME TO idx_users_address;
ALTER INDEX idx_orders_note RENAME TO idx_orders_memo;
