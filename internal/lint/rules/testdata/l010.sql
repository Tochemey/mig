-- +mig step: vacuum_full
-- +mig notx
VACUUM FULL users;

-- +mig step: cluster
CLUSTER users USING users_pkey;

-- +mig step: plain_vacuum_is_fine
-- +mig notx
VACUUM users;
