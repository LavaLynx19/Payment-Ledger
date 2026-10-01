-- +goose Up

-- Saga variant (A§9.5): a capture on the source shard records each credit
-- bound for another shard here, in the same local tx as the debit. The relay
-- applies it on the destination's shard and then deletes the row.
CREATE TABLE outbox (
    transfer_id uuid PRIMARY KEY REFERENCES transfers (id),
    dest_id     uuid NOT NULL,
    amount      bigint NOT NULL CHECK (amount > 0),
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- One debit and one credit Entry per Transfer. This is the relay's backstop
-- against applying a credit twice.
CREATE UNIQUE INDEX entries_transfer_direction ON entries (transfer_id, direction);

-- +goose Down
DROP INDEX entries_transfer_direction;
DROP TABLE outbox;
