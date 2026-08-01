-- +mig step: flagged
ALTER TYPE mood ADD VALUE 'curious';
UPDATE users SET mood = 'curious' WHERE mood IS NULL;

-- +mig step: other_values_are_fine
ALTER TYPE mood ADD VALUE 'restless';
UPDATE users SET mood = 'flat' WHERE mood IS NULL;

-- +mig step: adding_the_value_alone_is_fine
ALTER TYPE mood ADD VALUE 'settled';
