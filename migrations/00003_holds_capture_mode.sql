-- +goose Up

-- 'auto': the worker captures the Hold (CreateTransfer, TopUp, ...).
-- 'manual': PlaceHold, so the caller must CaptureHold or ReleaseHold it.
ALTER TABLE holds
    ADD COLUMN capture_mode text NOT NULL DEFAULT 'auto' CHECK (capture_mode IN ('auto', 'manual'));

DROP INDEX holds_capturable;
CREATE INDEX holds_capturable ON holds (id) WHERE status = 'active' AND capture_mode = 'auto';

-- +goose Down
DROP INDEX holds_capturable;
CREATE INDEX holds_capturable ON holds (id) WHERE status = 'active';
ALTER TABLE holds DROP COLUMN capture_mode;
