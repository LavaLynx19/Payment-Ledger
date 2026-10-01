-- +goose Up

-- References that can point at another shard can't be FKs (A§9.2). The
-- cross-shard checker verifies them instead (A§9.6). Always-local references
-- (source_id, entries.account_id, holds.transfer_id, debtor_wallet_id) keep
-- theirs.
ALTER TABLE transfers DROP CONSTRAINT transfers_dest_id_fkey;      -- destination on another shard
ALTER TABLE holds     DROP CONSTRAINT holds_dest_id_fkey;          -- destination on another shard
ALTER TABLE entries   DROP CONSTRAINT entries_transfer_id_fkey;    -- credit Entry lives with the destination
ALTER TABLE transfers DROP CONSTRAINT transfers_reverses_id_fkey;  -- reversal lives with the recipient

-- +goose Down
ALTER TABLE transfers ADD CONSTRAINT transfers_reverses_id_fkey FOREIGN KEY (reverses_id) REFERENCES transfers (id);
ALTER TABLE entries   ADD CONSTRAINT entries_transfer_id_fkey   FOREIGN KEY (transfer_id) REFERENCES transfers (id);
ALTER TABLE holds     ADD CONSTRAINT holds_dest_id_fkey         FOREIGN KEY (dest_id) REFERENCES accounts (id);
ALTER TABLE transfers ADD CONSTRAINT transfers_dest_id_fkey     FOREIGN KEY (dest_id) REFERENCES accounts (id);
