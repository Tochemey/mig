-- +mig step: flagged
ALTER TABLE users ADD COLUMN handle text UNIQUE;

-- +mig step: plain_is_fine
ALTER TABLE users ADD COLUMN notes text;

-- +mig step: new_table
CREATE TABLE audit (id int);

-- +mig step: unique_on_new_table_is_fine
ALTER TABLE audit ADD COLUMN handle text UNIQUE;
