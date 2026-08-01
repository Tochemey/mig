-- +mig step: needs_the_catalog
DELETE FROM sessions WHERE expires_at < now();
