-- +goose Up

-- Capture claims the oldest active Hold (ORDER BY id ... SKIP LOCKED). Without
-- this index it walks the primary key past every captured Hold, so each claim
-- gets slower as history grows (Rung 1 baseline: collapse above ~2k/s).
CREATE INDEX holds_capturable ON holds (id) WHERE status = 'active';

-- +goose Down
DROP INDEX holds_capturable;
