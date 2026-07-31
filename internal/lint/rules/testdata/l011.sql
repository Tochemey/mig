-- +mig step: flagged
REFRESH MATERIALIZED VIEW user_stats;

-- +mig step: concurrently_is_fine
REFRESH MATERIALIZED VIEW CONCURRENTLY org_stats;
