-- +mig step: flagged
ALTER TABLE users ALTER COLUMN id TYPE bigint;

-- +mig step: new_table
CREATE TABLE audit (id int);

-- +mig step: type_change_on_new_table_is_fine
ALTER TABLE audit ALTER COLUMN id TYPE bigint;

-- +mig step: qualified_new_table
CREATE TABLE app.audit2 (id int);

-- +mig step: same_name_in_another_schema_still_flagged
ALTER TABLE ops.audit2 ALTER COLUMN id TYPE bigint;

-- +mig step: alter_before_recreate_hits_live_rows
ALTER TABLE cache ALTER COLUMN v TYPE bigint;

-- +mig step: drop_cache
DROP TABLE cache;

-- +mig step: recreate_cache
CREATE TABLE cache (v bigint);

-- +mig step: grant_audit
GRANT SELECT ON audit TO application;

-- +mig step: grant_app_audit2
GRANT SELECT ON app.audit2 TO application;

-- +mig step: grant_cache
GRANT SELECT ON cache TO application;
