-- +goose Up

-- No CHECK (posted >= 0) on Wallets: Rung 1 must be able to break that
-- invariant so the checker, not the database, detects the double-spend.
CREATE TABLE accounts (
    id               uuid PRIMARY KEY,
    kind             text NOT NULL CHECK (kind IN ('wallet', 'system')),
    subtype          text CHECK (subtype IN ('funding', 'receivable')),
    debtor_wallet_id uuid REFERENCES accounts (id),
    normal_balance   text NOT NULL CHECK (normal_balance IN ('debit', 'credit')),
    posted           bigint NOT NULL DEFAULT 0,
    version          bigint NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((kind = 'wallet') = (subtype IS NULL)),
    CHECK ((coalesce(subtype, '') = 'receivable') = (debtor_wallet_id IS NOT NULL))
);

CREATE UNIQUE INDEX accounts_receivable_per_debtor
    ON accounts (debtor_wallet_id) WHERE subtype = 'receivable';

CREATE TABLE transfers (
    id          uuid PRIMARY KEY,
    type        text NOT NULL CHECK (type IN ('p2p', 'topup', 'withdrawal', 'reversal', 'repayment', 'receivable')),
    source_id   uuid NOT NULL REFERENCES accounts (id),
    dest_id     uuid NOT NULL REFERENCES accounts (id),
    amount      bigint NOT NULL CHECK (amount > 0),
    status      text NOT NULL CHECK (status IN ('pending', 'posted', 'failed')),
    reverses_id uuid REFERENCES transfers (id),
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    posted_at   timestamptz,
    CHECK (source_id <> dest_id)
);

CREATE TABLE holds (
    id              uuid PRIMARY KEY,
    transfer_id     uuid NOT NULL UNIQUE REFERENCES transfers (id),
    source_id       uuid NOT NULL REFERENCES accounts (id),
    dest_id         uuid NOT NULL REFERENCES accounts (id),
    amount          bigint NOT NULL CHECK (amount > 0),
    captured_amount bigint CHECK (captured_amount > 0 AND captured_amount <= amount),
    expires_at      timestamptz, -- NULL = never expires (system-initiated)
    status          text NOT NULL CHECK (status IN ('active', 'captured', 'released', 'expired')),
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX holds_active_by_source ON holds (source_id) WHERE status = 'active';
CREATE INDEX holds_active_by_expiry ON holds (expires_at)
    WHERE status = 'active' AND expires_at IS NOT NULL;

CREATE TABLE entries (
    id              uuid PRIMARY KEY,
    transfer_id     uuid NOT NULL REFERENCES transfers (id),
    account_id      uuid NOT NULL REFERENCES accounts (id),
    direction       text NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount          bigint NOT NULL CHECK (amount > 0),
    balance_after   bigint NOT NULL,
    account_version bigint NOT NULL,
    created_at      timestamptz NOT NULL,
    UNIQUE (account_id, account_version)
);

CREATE INDEX entries_by_account_time ON entries (account_id, created_at, account_version);
CREATE INDEX entries_by_transfer ON entries (transfer_id);

-- The key row is inserted before its Transfer (A§5 Accept step 1), so the
-- FK is checked at commit.
CREATE TABLE idempotency_keys (
    key          text PRIMARY KEY,
    request_hash bytea NOT NULL,
    transfer_id  uuid NOT NULL REFERENCES transfers (id) DEFERRABLE INITIALLY DEFERRED,
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX idempotency_keys_by_age ON idempotency_keys (created_at);

-- +goose Down
DROP TABLE idempotency_keys;
DROP TABLE entries;
DROP TABLE holds;
DROP TABLE transfers;
DROP TABLE accounts;
