-- +mig step: create_metrics
CREATE TABLE metrics (id bigint PRIMARY KEY);

-- +mig step: create_events
CREATE TABLE events (id bigint PRIMARY KEY, note text);
