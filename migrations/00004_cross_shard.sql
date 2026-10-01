-- +goose Up

-- 2PC commit decisions (A§9.4). A row means "commit every participant of
-- gid". The resolver commits a prepared gid whose decision exists and rolls
-- back one that has none (presumed abort).
CREATE TABLE decisions (
    gid        text PRIMARY KEY,
    decided_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- An idempotency key lives on shard hash(key), and its Transfer on the source
-- shard, so the FK can't span them (A§9.2). The cross-shard checker
-- verifies key → existing Transfer instead (invariant 6).
ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_transfer_id_fkey;

-- +goose Down
ALTER TABLE idempotency_keys
    ADD CONSTRAINT idempotency_keys_transfer_id_fkey
    FOREIGN KEY (transfer_id) REFERENCES transfers (id) DEFERRABLE INITIALLY DEFERRED;
DROP TABLE decisions;
