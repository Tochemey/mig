-- +mig step: constant_default_rewrites_before_11
ALTER TABLE users ADD COLUMN score int DEFAULT 0;
