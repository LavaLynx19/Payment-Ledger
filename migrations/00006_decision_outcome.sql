-- +goose Up

-- The decision row arbitrates a 2PC (A§9.4): the coordinator inserts
-- 'commit', the resolver 'abort', both ON CONFLICT DO NOTHING, and the first
-- row wins. That keeps presumed abort safe against a slow coordinator.
ALTER TABLE decisions
    ADD COLUMN outcome text NOT NULL DEFAULT 'commit' CHECK (outcome IN ('commit', 'abort'));
ALTER TABLE decisions ALTER COLUMN outcome DROP DEFAULT;

-- +goose Down
ALTER TABLE decisions DROP COLUMN outcome;
