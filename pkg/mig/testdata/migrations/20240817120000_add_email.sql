-- +mig step: add_email
ALTER TABLE users ADD COLUMN email text;
