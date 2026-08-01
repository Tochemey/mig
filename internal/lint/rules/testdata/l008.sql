-- +mig step: flagged
ALTER TABLE users ADD CONSTRAINT users_pk PRIMARY KEY (id);

-- +mig step: inline_flagged
ALTER TABLE orders ADD COLUMN id2 bigint PRIMARY KEY;

-- +mig step: using_index_is_fine
ALTER TABLE invoices ADD CONSTRAINT invoices_pk PRIMARY KEY USING INDEX invoices_key;

-- +mig step: new_table
CREATE TABLE audit (id int);

-- +mig step: key_on_new_table_is_fine
ALTER TABLE audit ADD CONSTRAINT audit_pk PRIMARY KEY (id);

-- +mig step: grant_audit
GRANT SELECT ON audit TO application;
