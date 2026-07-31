-- +mig step: flagged
ALTER TABLE orders ADD CONSTRAINT orders_user_fk FOREIGN KEY (user_id) REFERENCES users (id);

-- +mig step: not_valid_is_fine
ALTER TABLE orders ADD CONSTRAINT orders_org_fk FOREIGN KEY (org_id) REFERENCES orgs (id) NOT VALID;

-- +mig step: new_table
CREATE TABLE audit (user_id bigint);

-- +mig step: key_on_new_table_is_fine
ALTER TABLE audit ADD CONSTRAINT audit_user_fk FOREIGN KEY (user_id) REFERENCES users (id);
