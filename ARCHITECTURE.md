# payment-ledger — Architecture

## 1. Overview

This file covers **how** payment-ledger is built. Requirements live in [README.md](./README.md) and domain terms in [CONTEXT.md](./CONTEXT.md). Neither is restated here.

It's a Go service over a hand-built Postgres ledger. An API process accepts requests and places **Holds**. A worker process **Captures** them into balanced **Entries**. An invariant checker audits the ledger, and a k6 + fault-injection harness drives each **Rung**. Rungs 1-3 run on a single Postgres. Rung 4 compares sharding our own engine against TigerBeetle.

## 2. Tech research

Scoring criteria: M4 Pro footprint, ecosystem maturity, learning value.

### 2.1 Language
| Option | Pros | Cons |
|---|---|---|
| **Go** ✅ | Small footprint; stdlib HTTP/SQL; goroutines make races easy to reproduce; common in payments infra | Invariants rely on discipline, not types |
| JVM (Java/Kotlin) | Most mature; bank default | Heavy memory/startup; framework pull |
| Rust | Strongest compile-time guarantees | Steep; async + DB friction slows the rung loop |

**Choice:** Go. It's light, mature, and makes concurrency bugs visible, which is what Rung 1 needs.

### 2.2 Storage engine
| Option | Pros | Cons |
|---|---|---|
| **PostgreSQL (Rungs 1-3)** ✅ | MVCC, row locks, SERIALIZABLE; mature; runs locally; multiple instances can act as shards | Ledger logic is ours to build (intended) |
| TigerBeetle | Purpose-built double-entry; pending/post/void, timeouts, idempotent IDs, balance limits | Solves the rung problems in-engine, so nothing is learned |
| SQLite | Zero ops | A single writer hides Rung 1 races; no path to Rung 4 |

**Choice:** Postgres now, TigerBeetle as the Rung 4 comparison. See Decision Log → *Hand-built Postgres ledger before TigerBeetle*.

### 2.3 Balance source of truth
| Option | Pros | Cons |
|---|---|---|
| **Materialized `posted` + `version` on Account, same tx as Entries** ✅ | O(1) reads and funds checks | It's the lost-update and hot-account target; could drift if a code path skips it |
| Derived from Entries | Can't disagree with Entries | Reads and funds checks get slower as history grows |
| Async projection | Scales reads | Breaks strict read-your-writes |

**Choice:** Materialized. Drift is caught by the invariant checker (§8).

### 2.4 Concurrency control (Rung 1 fix hypothesis)
| Option | Pros | Cons |
|---|---|---|
| **Optimistic `version` CAS + retry** ✅ | No held locks in the app; conflicts are explicit | Retry storms under contention (Rung 3) |
| `SELECT … FOR UPDATE` | Simple; obviously correct | Serializes writers; lock-order deadlocks |
| Conditional `UPDATE … WHERE available >= x` | One statement | Guard hidden in SQL; Available isn't a stored column here |
| SERIALIZABLE + retry | No lock reasoning | Throughput collapses on hot Accounts |

**Choice:** CAS is the hypothesis. The Rung 1 retro confirms or replaces it. Holds also bump the version (Decision Log → *Placing a Hold bumps the Account version*).

### 2.5 Hold expiry
| Option | Pros | Cons |
|---|---|---|
| **Lazy at read + sweeper** ✅ | Available is always exact | Available is computed, not stored; funds checks must filter by `expires_at` |
| Sweeper only | Simple | Available is understated until the sweep runs |
| Expire on touch | Exact, no clock in reads | Idle Accounts keep stale Holds; heavier write path |

### 2.6 Capture execution
| Option | Pros | Cons |
|---|---|---|
| **Worker process, Postgres queue (`FOR UPDATE SKIP LOCKED`)** ✅ | No new dep; crash recovery is natural | Polling latency; one table serves as queue and ledger |
| In-process goroutine | Lowest latency | A crash leaves orphaned PENDING Transfers that still need a sweeper |
| Broker (Kafka/NATS) | Realistic backbone | New dep; dual-write needs an outbox; heavy footprint |

### 2.7 Idempotency store
| Option | Pros | Cons |
|---|---|---|
| **Postgres table, same tx as the Hold; 24h retention** ✅ | No key without a Hold and no Hold without a key | The table needs purging |
| Redis | Fast | Not atomic with the ledger; new dep |

A retry after 24h is treated as a new request. That window is a documented client contract.

### 2.8 Receivable placement
| Option | Pros | Cons |
|---|---|---|
| **Per-debtor receivable System account** ✅ | Clean double-entry; per-debtor queries are trivial | The debit-block check reads a second row, so it needs a version bump on the debtor Wallet (Decision Log) |
| Single global account | Few accounts | Per-debtor balance becomes a GROUP BY; hot account |
| Column on Wallet row | Race-free for free | Not a real Account, so the audit special-cases it |

### 2.9 Transport
| Option | Pros | Cons |
|---|---|---|
| **connect-go** ✅ | One handler serves gRPC, gRPC-Web, and Connect (HTTP/JSON); small | Less common than grpc-go; needs buf codegen |
| grpc-go + grpc-gateway | Most widely used | Two deps + codegen; extra hop |
| grpc-go + hand-written HTTP | No gateway dep | Two surfaces that drift apart |

### 2.10 Postgres driver
| Option | Pros | Cons |
|---|---|---|
| **pgx native + pgxpool** ✅ | De facto Go driver; fastest; full PG types; built-in pool | pgx-specific API |
| database/sql + pgx adapter | Swappable interface | Loses pgx features |
| lib/pq | Classic | Maintenance mode |

### 2.11 IDs
| Option | Pros | Cons |
|---|---|---|
| **UUIDv7** ✅ | No coordination; time-ordered inserts; 128-bit, so it maps to TigerBeetle IDs | 16-byte keys; leaks time |
| bigserial | Compact | Single sequence breaks at Rung 4 |
| Snowflake 64-bit | Compact, shard-safe | Node-ID assignment; doesn't fill TB's 128 bits |

Generated in Go with `github.com/google/uuid` (`NewV7`), so the app knows every ID before insert, which Rung 4 routing and TigerBeetle both need. Rejected: Postgres `uuidv7()`, which has no dep but only produces IDs at insert time and requires PG ≥ 18.

### 2.12 Point-in-time balance and ordering
| Option | Pros | Cons |
|---|---|---|
| **`balance_after` on each Entry** ✅ | Balance at T is an index seek | Requires per-Account serialized posting (CAS already gives this) |
| Sum Entries ≤ T | Nothing extra stored | Scan grows with history |
| Periodic snapshots | Bounded scans | Snapshot job and cut correctness |

Ordering: Entries are stamped with `clock_timestamp()` *after* the version CAS succeeds, and they store the resulting `account_version` as a tiebreaker that clock skew can't break. See Decision Log → *CAS before Entries, version as tiebreaker*.

### 2.13 Migrations
| Option | Pros | Cons |
|---|---|---|
| **goose** ✅ | Mature; SQL migrations; embeddable in the binary | One dep |
| Plain SQL + custom runner | No dep | No locking/down-migrations unless we write them |
| golang-migrate | Widely used | Heavier dep tree |

### 2.14 Runtime
| Option | Pros | Cons |
|---|---|---|
| **Docker Compose** ✅ | One command for N Postgres + TB + toxiproxy; per-container kill -9; reproducible | VM overhead on macOS, so latencies are relative only |
| Native (Homebrew) | Cleaner latency | Manual multi-instance setup |

### 2.15 Harness
| Concern | Choice | Rejected |
|---|---|---|
| Load | **k6** (`k6/net/grpc`, plaintext h2c) | Custom Go harness (build percentiles ourselves); ghz/vegeta (stateless) |
| Faults | **kill -9 scripts + env-gated in-code failpoints + toxiproxy** | — (all three chosen; they cover random, deterministic, and network faults) |
| Invariant checker | **Go binary with SQL checks**, on an interval and on demand | pg_cron (extension dep; awkward across shards) |

## 3. Chosen stack

| Layer | Choice |
|---|---|
| Language | Go |
| API | connect-go over h2c (gRPC + Connect HTTP/JSON) |
| Storage | PostgreSQL (Rungs 1-3; version pinned at PLAN); TigerBeetle added at Rung 4 |
| Driver | pgx v5 + pgxpool |
| Migrations | goose (SQL files, embedded) |
| Runtime | Docker Compose |
| Load / faults | k6, kill -9 scripts, failpoints, toxiproxy |

**Dependencies** (versions are pinned at PLAN time; latest stable):

| Dep | Kind | Why |
|---|---|---|
| `connectrpc.com/connect` | Go | Dual-protocol API from one handler |
| `google.golang.org/protobuf` | Go | Protobuf runtime needed by connect-go |
| `buf` + `protoc-gen-go` + `protoc-gen-connect-go` | Go `tool` directive in go.mod | Codegen from `.proto` (`go tool buf generate`). `buf curl` doubles as the gRPC/Connect smoke client, so grpcurl isn't needed. Versions are tracked in go.mod and nothing is installed globally. |
| `github.com/jackc/pgx/v5` | Go | Postgres driver + pool |
| `github.com/google/uuid` | Go | Generates UUIDv7 IDs in the app |
| `github.com/pressly/goose/v3` | Go | Migrations |
| PostgreSQL 18 | Container | Ledger storage |
| k6 | Docker image (`grafana/k6`), run by the harness | Load |
| toxiproxy | Container, driven via its HTTP API | Network faults; no Go client dep |
| TigerBeetle + Go client | Container + Go | Rung 4 only |

Plaintext HTTP/2 (h2c) comes from the stdlib `http.Protocols` (`SetUnencryptedHTTP2`), available in Go ≥ 1.24, so there's no `x/net` dep.

Service auth uses static bearer tokens read from the env var `LEDGER_SERVICE_TOKENS` and checked by a connect interceptor. No auth dependency.

## 4. Data model

Amounts are positive `bigint` minor units. Each Entry has a **direction** (`debit` or `credit`), and each Account has a **normal balance** that says which direction increases its `posted`. This makes per-direction and per-type reports (total debits, credit counts) plain filters instead of sign arithmetic.

| Account | Normal balance | Meaning of `posted` |
|---|---|---|
| Wallet | credit | What the platform owes the user |
| System: funding | debit | Simulated cash at bank |
| System: receivable | debit | What the debtor owes |

Every Transfer debits its source and credits its destination. IDs are UUIDv7, generated in the app.

```sql
accounts(
  id uuid PK, kind text CHECK (kind IN ('wallet','system')),
  subtype text NULL,               -- system only: 'funding' | 'receivable'
  debtor_wallet_id uuid NULL FK accounts,  -- set iff subtype='receivable'
  normal_balance text CHECK (normal_balance IN ('debit','credit')),
  posted bigint NOT NULL DEFAULT 0,  -- in the Account's normal direction
  version bigint NOT NULL DEFAULT 0,
  created_at timestamptz)

transfers(
  id uuid PK, type text,           -- 'p2p'|'topup'|'withdrawal'|'reversal'|'repayment'|'receivable'
  source_id uuid FK, dest_id uuid FK, amount bigint CHECK (amount > 0),
  status text,                     -- 'pending'|'posted'|'failed'
  reverses_id uuid NULL FK transfers,
  created_at timestamptz, posted_at timestamptz NULL)

holds(
  id uuid PK, transfer_id uuid UNIQUE FK, source_id uuid FK, dest_id uuid FK,
  amount bigint, captured_amount bigint NULL,
  expires_at timestamptz NULL,     -- NULL = never expires (system-initiated: reversal, receivable)
  status text,                     -- 'active'|'captured'|'released'|'expired'
  capture_mode text,               -- 'auto': the worker captures it | 'manual': PlaceHold, the caller captures or releases it
  INDEX (source_id) WHERE status='active',                          -- funds check: active Holds per Account
  INDEX (id) WHERE status='active' AND capture_mode='auto')         -- capture claim: oldest auto Hold without scanning history

entries(
  id uuid PK, transfer_id uuid FK, account_id uuid FK,
  direction text CHECK (direction IN ('debit','credit')),
  amount bigint CHECK (amount > 0),
  balance_after bigint, account_version bigint,
  created_at timestamptz,          -- clock_timestamp() after CAS
  UNIQUE (account_id, account_version),
  INDEX (account_id, created_at))

idempotency_keys(
  key text PK, request_hash bytea, transfer_id uuid FK,
  created_at timestamptz)          -- purged after 24h
```

**Derived values**
- **Available balance**(A) = `posted` − Σ `holds.amount` where `source_id = A`, `status = 'active'`, and (`expires_at IS NULL` or `expires_at > clock_timestamp()`).
- **Receivable** owed by Wallet W = `posted` + Σ active Holds sourced from W's receivable System account. Pending shortfalls count, so it is *open* (owed > 0) from the moment a Reversal is accepted.

**Invariants** (all enforced by the checker in §8)
1. Σ debit amounts = Σ credit amounts, both over the whole ledger and per Transfer.
2. `accounts.posted` = Σ normal-direction Entries − Σ opposite-direction Entries = its latest Entry's `balance_after`.
3. A Wallet never goes negative at any point in its history. That means current `posted` ≥ 0, current Available balance ≥ 0, and no Wallet Entry has `balance_after` < 0. The history part matters because later credits can refill an overspent Wallet before a current-state check runs.
4. Per Account, `account_version` values on Entries strictly increase and are unique.
5. Every posted Transfer has exactly one captured Hold, and its Entries post exactly the Hold's `captured_amount`. `transfers.amount` stays the requested amount. No Hold is both captured and expired.
6. Every idempotency key maps to exactly one Transfer.
7. A reversed Transfer has at most one reversal and one receivable Transfer, and their amounts sum to its posted amount. A receivable System account never goes below zero, now or in its history, so no Receivable is ever overpaid.

## 5. Write paths

**Failpoints.** `FAILPOINTS=name[=probability],…` (e.g. `capture.after_entries=0.001`) enables the named points below. Unknown names fail at startup. A firing point exits the process with `os.Exit(137)`. That matches kill -9: inside a container the process is PID 1 and can't SIGKILL itself, cleanup is skipped, and Postgres rolls back the open tx when the connection drops. Compose restarts api and worker on failure.

All paths run inside a single transaction under READ COMMITTED. A "CAS" is `UPDATE accounts SET …, version = version + 1 WHERE id = $1 AND version = $v`. If it affects 0 rows, the tx rolls back and retries with a fresh read (bounded retries, then `ABORTED`). When a tx touches more than one Account row, it updates them in ascending `id` order to avoid deadlocks.

**Accept (CreateTransfer / PlaceHold / TopUp / Withdraw / Repay)**
1. `INSERT INTO idempotency_keys … ON CONFLICT DO NOTHING`. On conflict: if the hash matches, return the existing Transfer (PENDING or final); otherwise return `IDEMPOTENCY_MISMATCH`. If the original request is still in flight, the unique index makes this insert wait for the original to commit, and then it returns the same transfer ID.
2. Read the source Account (`posted`, `version`) and its active, unexpired Holds.
3. Wallet source only: reject with `RECEIVABLE_OPEN` if the Wallet's receivable is open and the type isn't `repayment`. Reject with `INSUFFICIENT_FUNDS` if Available < amount.
4. CAS the source Account (version bump only, no balance change).
5. Insert `transfers` (pending) and `holds` (active, `expires_at` = now + TTL). Commit, then respond PENDING.

- `failpoint accept.before_commit`: the tx aborts and nothing persists. The client retries and is accepted as new.
- `failpoint accept.after_commit`: the client never gets a response, retries, and receives the same ID.

**Capture (worker)**
1. Claim: `SELECT … FROM holds WHERE status='active' AND capture_mode='auto' AND (expires_at IS NULL OR expires_at > clock_timestamp()) … FOR UPDATE SKIP LOCKED LIMIT n`. Manual Holds (PlaceHold) are never claimed.
2. `UPDATE holds SET status='captured' WHERE id=$1 AND status='active' AND (expires_at IS NULL OR expires_at > clock_timestamp())`. If this affects 0 rows, expiry won the race, so the Transfer is marked failed. First commit wins.
3. CAS the source and destination in `id` order, debiting the source and crediting the destination. Each moves `posted` according to that Account's normal balance. Then insert the debit and credit Entries with `balance_after`, `account_version` = new version, and `created_at = clock_timestamp()`.
4. Set the Transfer to `posted`. Commit.

**CaptureHold** (manual Holds only; runs synchronously in the API request)
1. Lock the Hold (`SELECT … FOR UPDATE`), which serializes it against Release and other Captures. Then claim the Idempotency-Key against the Hold's Transfer. A replay returns the current Transfer and Hold.
2. Reject if the Hold is not active (`HOLD_NOT_ACTIVE`), if it's an auto Hold (`INVALID_REQUEST`), or if the amount isn't between 1 and the Hold amount.
3. Run Capture steps 2-4 for the requested amount (full amount by default). `captured_amount` records what was posted, the remainder is released in the same tx, and `transfers.amount` keeps the requested amount. If expiry won step 2, return `HOLD_EXPIRED`.

- `failpoint capture.after_claim` and `failpoint capture.after_entries` crash before commit, so the tx aborts, the row lock is released, and another worker re-claims.
- `failpoint capture.after_commit` crashes after commit. Nothing is lost, and the next claim skips the already-captured Hold.

**Release / expiry**
- `ReleaseHold` (manual Holds only; lock and key claim as in CaptureHold): `UPDATE holds SET status='released' WHERE status='active'`, then CAS the source. The Transfer fails. `failpoint release.before_commit` sits here.
- Sweeper: batch `UPDATE holds SET status='expired' WHERE status='active' AND expires_at <= clock_timestamp()`, then mark their Transfers failed. This only finalizes records, because reads already ignore expired Holds. Holds with no expiry are never touched. `failpoint sweeper.mid_batch` sits here.
- Purge: the same loop deletes `idempotency_keys` older than 24h, in batches. `failpoint purge.mid_batch` sits here.

**Reversal of Transfer T (A → B, amount X)**
Only posted P2P Transfers can be reversed, once. X is the amount actually posted (the Hold's `captured_amount`). In one tx:
0. Lock T (`SELECT … FOR UPDATE`), then claim the key. A replay returns T's existing reversal Transfers. Reject with `TRANSFER_NOT_REVERSIBLE` if T isn't a posted P2P Transfer or already has a reversal.
1. Compute r = min(X, B's Available). When r = 0 only the receivable Transfer is created, and when X − r = 0 only the reversal Transfer is.
2. Create a `reversal` Transfer and Hold for B → A of r. Create a `receivable` Transfer and Hold for B's receivable System account → A of X − r, creating that System account if it doesn't exist yet.
3. CAS B's Wallet and B's receivable account, so the debtor Wallet's version is bumped even when r = 0.

Both Holds have no expiry (`expires_at` NULL). See Decision Log → *System-initiated Holds never expire*. The worker Captures both Transfers normally.
- `failpoint reversal.before_commit`: the tx aborts and nothing persists.
- `failpoint reversal.after_commit`: the caller retries with the same key and gets the same Transfers back.

**Repayment**
An Accept of type `repayment` from Wallet W → W's receivable System account. The amount must be ≤ the owed amount minus Repayments still pending, so two in-flight Repayments can't overpay. A Wallet with no receivable account gets `INVALID_REQUEST`. It's exempt from the `RECEIVABLE_OPEN` check. Every other debit from a Wallet (P2P, PlaceHold, Withdraw) is blocked while the Wallet owes anything.

**Rung 1 naive variant**
Steps 2-5 of Accept without the CAS in step 4. Two concurrent Accepts both pass the funds check, which is the double-spend.

## 6. System topology

```
 k6 ──gRPC/h2c──┐          UI ──Connect HTTP/JSON──┐
                ▼                                   ▼
        ┌────────────────── api (connect-go) ──────────────────┐
        │ auth interceptor → service layer → pgxpool           │
        └──────────────────────────┬───────────────────────────┘
                                   │ (toxiproxy in between for network faults)
                                   ▼
                          ┌──────────────────┐
     worker ×N ─────────▶ │   PostgreSQL     │ ◀───── checker (interval + on demand)
     (capture, sweeper)   └──────────────────┘
```

All boxes are Compose services on one machine. The worker processes run Capture and the sweeper as separate loops. The harness script runs: bring up the stack → migrate → seed Accounts → k6 scenario → fault injection → checker → report.

**Rung 4 (deferred):** N Postgres shards keyed by Account and/or a TigerBeetle cluster. The cross-shard protocol and the strict-reads vs never-negative tradeoff are decided at that Rung and appended to the Decision Log.

## 7. API contract

Package `ledger.v1`, service `LedgerService`. It's served as gRPC and as Connect HTTP/JSON (`POST /ledger.v1.LedgerService/<Method>`). Side-effect-free reads are marked `idempotency_level = NO_SIDE_EFFECTS`, which also enables GET.

| RPC | Request (key fields) | Response |
|---|---|---|
| CreateTransfer | source_id, dest_id, amount, hold_ttl? | transfer (id, status=PENDING) |
| GetTransfer | id | transfer |
| PlaceHold | source_id, dest_id, amount, ttl | hold, transfer |
| CaptureHold | hold_id, amount? (≤ hold) | transfer |
| ReleaseHold | hold_id | hold |
| ReverseTransfer | transfer_id | reversal transfer, receivable transfer? |
| Repay | wallet_id, amount | transfer |
| TopUp / Withdraw | wallet_id, amount | transfer |
| GetBalance | account_id | posted, available, active holds, receivable_owed |
| GetBalanceAt | account_id, at | posted as of `at` |
| ListEntries | account_id, from, to, cursor, limit | entries, next_cursor |
| ListReceivables | min_age?, cursor | receivables (debtor, owed, age) |

**Read semantics**
- `GetBalanceAt`: `balance_after` of the Account's latest Entry with `created_at ≤ at`, ties broken by `account_version`. 0 if there's none.
- `ListEntries`: Entries in `account_version` order, optionally bounded by `from ≤ created_at < to`. The cursor is opaque and encodes the last version returned. `limit` defaults to 100 and is capped at 1000.
- `ListReceivables`: Wallets that owe more than 0, including shortfalls whose Holds aren't captured yet, ordered by debtor id with an opaque cursor. `opened_at` is when the oldest receivable Transfer was created since the debtor last owed nothing. `min_age` keeps only Receivables opened at least that long ago.

**Headers**
- `Authorization: Bearer <service token>` on every call.
- `Idempotency-Key` is required on every mutating RPC. The request hash is SHA-256 of the RPC procedure name (e.g. `/ledger.v1.LedgerService/TopUp`), a zero byte, and the deterministic protobuf encoding of the request. TopUp, Withdraw and Repay share one message shape, so including the procedure turns a key reused across RPCs into `IDEMPOTENCY_MISMATCH` instead of a false match. Keys are global, not per RPC.

**Errors** use a Connect code, a `reason` detail, and a message written for humans. `{…}` placeholders are filled from the request or ledger state.

| reason | Connect code | HTTP | Message |
|---|---|---|---|
| INSUFFICIENT_FUNDS | FAILED_PRECONDITION | 400 | "Available balance is {available}, which is less than the {amount} requested." |
| RECEIVABLE_OPEN | FAILED_PRECONDITION | 400 | "This wallet owes {owed} from a reversed transfer. Only repayments are allowed until it's paid off." |
| HOLD_EXPIRED | FAILED_PRECONDITION | 400 | "This hold expired at {expires_at} and can no longer be captured." |
| HOLD_NOT_ACTIVE | FAILED_PRECONDITION | 400 | "This hold has already been {status}." |
| IDEMPOTENCY_MISMATCH | ALREADY_EXISTS | 409 | "This Idempotency-Key was already used with a different request. Use a new key for a new request." |
| CONFLICT_RETRIES_EXHAUSTED | ABORTED | 409 | "The account was busy and the request couldn't be applied. Retry with the same Idempotency-Key." |
| NOT_FOUND | NOT_FOUND | 404 | "No {resource} found with id {id}." |
| INVALID_REQUEST | INVALID_ARGUMENT | 400 | Names the field and rule, e.g. "amount must be greater than 0." |
| TRANSFER_NOT_REVERSIBLE | FAILED_PRECONDITION | 400 | Names the reason, e.g. "This transfer has already been reversed." |
| UNAUTHENTICATED | UNAUTHENTICATED | 401 | "Missing or invalid service token." |

## 8. Rung mechanics

| Rung | Expected failure | Harness scenario | Fix |
|---|---|---|---|
| 1 Lost update / double-spend | Naive Accept lets concurrent Holds exceed Available | k6: many VUs send Transfers from a few Wallets; checker invariant 3 fails | Hypothesis: version CAS (§5). Confirmed in the retro |
| 2 Crash mid-Transfer | Orphaned PENDING Transfers, duplicate or lost money on retry | Each failpoint enabled in turn + random kill -9 of api/worker/postgres under load; clients retry with the same key | Idempotency table + SKIP LOCKED re-claim + sweeper. Verified per failpoint |
| 3 Hot Account contention | CAS retry storm on the funding System account (every Top-up) and on popular destinations | k6: a high share of traffic touches one Account; watch retries and p99 | Deferred. Decided at the Rung, then logged |
| 4 Sharding + cross-shard | Transfers spanning shards can't use one tx | Shards via Compose; toxiproxy partitions between app and shards | Deferred. Compare own shards vs TigerBeetle |

**Checker** (Go binary, `--once` or `--interval`): runs invariants 1-6 from §4 as SQL and exits non-zero on any violation. Every Rung's pass criteria include a clean run.

TPS targets are multiples of the Rung 1 baseline (see README), recorded in each Rung retro.

## Decision Log

### Hand-built Postgres ledger before TigerBeetle
TigerBeetle already provides pending transfers, timeouts, idempotent IDs, and balance limits, so building on it would skip the problems Rungs 1-3 exist to teach. We hand-build on Postgres and bring TigerBeetle in at Rung 4 as a comparison against sharding our own engine.

### Rung 1 ships an intentionally racy Accept
The first Accept deliberately omits the version CAS so the harness can show a real double-spend under load. Don't "fix" it before the Rung 1 retro records the failure. The fix then lands as a separate change.

### Placing a Hold bumps the Account version
Holds don't change `posted`, but two concurrent Holds must still conflict, or both pass the funds check. So every Hold placement CASes the source Account's version. That makes Hold placement and posting share one contention point, and Rung 3 is expected to hit it.

### Receivable creation bumps the debtor Wallet version
The Receivable lives on a separate System account, so a Hold on the debtor Wallet could otherwise race the Reversal that opens it. The Reversal tx always CASes the debtor Wallet (even when nothing is recoverable), so a concurrent Hold conflicts and re-reads the now-open Receivable.

### CAS before Entries, version as tiebreaker
Entries are inserted only after the Account CAS succeeds, and they're stamped with `clock_timestamp()`. A retried tx re-stamps, so per-Account timestamps follow commit order. Each Entry also stores `account_version`, so ordering survives clock skew. Point-in-time queries use `created_at ≤ T` and break ties by version.

### Rung gates: total ops/s, and p99 only without faults
Rung 2's scenario mixes P2P with Holds and reversals, and it deliberately crashes processes. A literal "2,000 posted/s at p99 ≤ 7.6 ms" would have failed for definitional reasons: P2P is only 80% of the mix, and a crash always delays in-flight calls, pushing p99 to 22–49 ms on api crashes and about 1 s when Postgres restarts. So throughput is total ops/s at the target rate, the p99 gate applies to the fault-free run, and crash-time p99 is recorded as observed.

### System-initiated Holds never expire
Every other Hold expires, but Reversal and receivable Holds have no expiry. If the worker is down or slow, a correction must not silently fail and leave a mistaken Transfer uncorrected. The cost is that the debtor's funds stay held until the worker recovers.
