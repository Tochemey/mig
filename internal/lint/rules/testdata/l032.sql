-- +mig step: create_metrics
CREATE TABLE metrics (id bigint PRIMARY KEY);

-- +mig step: grant_everything_that_exists
GRANT SELECT ON ALL TABLES IN SCHEMA public TO application;

-- +mig step: create_events
CREATE TABLE events (id bigint PRIMARY KEY, note text);

-- +mig step: grant_events
GRANT SELECT, INSERT ON events TO application;

-- +mig step: flagged
CREATE TABLE audit (id bigint PRIMARY KEY, note text);
