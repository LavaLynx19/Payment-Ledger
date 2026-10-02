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
8. (Rung 4) Every reference that can cross shards resolves on the shard its id routes to: `transfers.dest_id`, `holds.dest_id`, `entries.transfer_id`, `transfers.reverses_id`, `idempotency_keys.transfer_id`. No 2PC is left in doubt: no prepared tx remains once the ledger is idle.

## 5. Write paths

**Failpoints.** `FAILPOINTS=name[=probability],…` (e.g. `capture.after_entries=0.001`) enables the named points below. Unknown names fail at startup. A firing point exits the process with `os.Exit(137)`. That matches kill -9: inside a container the process is PID 1 and can't SIGKILL itself, cleanup is skipped, and Postgres rolls back the open tx when the connection drops. Compose restarts api and worker on failure.

All paths run inside a single transaction under READ COMMITTED. A "CAS" is `UPDATE accounts SET …, version = version + 1 WHERE id = $1 AND version = $v`. If it affects 0 rows, the tx rolls back and retries with a fresh read (bounded retries, then `ABORTED`). When a tx touches more than one Account row, it updates them in ascending `id` order to avoid deadlocks.

**Accept (CreateTransfer / PlaceHold / TopUp / Withdraw / Repay)**
1. `INSERT INTO idempotency_keys … ON CONFLICT DO NOTHING`. On conflict: if the hash matches, return the existing Transfer (PENDING or final); otherwise return `IDEMPOTENCY_MISMATCH`. If the original request is still in flight, the unique index makes this insert wait for the original to commit, and then it returns the same transfer ID.
2. Read the source Account (`posted`, `version`) and its active, unexpired Holds.
3. Wallet source only: reject with `RECEIVABLE_OPEN` if the Wallet's receivable is open and the type isn't `repayment`. Reject with `INSUFFICIENT_FUNDS` if Available < amount.
4. Wallet source only: CAS the source Account (version bump only, no balance change). A System-account source (funding, receivable) is not CASed, because there's no funds check for the bump to protect (Decision Log → *Rung 3: version checks only where a funds check needs them*).
5. Insert `transfers` (pending) and `holds` (active, `expires_at` = now + TTL). Commit, then respond PENDING.

- `failpoint accept.before_commit`: the tx aborts and nothing persists. The client retries and is accepted as new.
- `failpoint accept.after_commit`: the client never gets a response, retries, and receives the same ID.

**Capture (worker)**
1. Claim: `SELECT … FROM holds WHERE status='active' AND capture_mode='auto' AND (expires_at IS NULL OR expires_at > clock_timestamp()) … FOR UPDATE SKIP LOCKED LIMIT n`. Manual Holds (PlaceHold) are never claimed.
2. `UPDATE holds SET status='captured' WHERE id=$1 AND status='active' AND (expires_at IS NULL OR expires_at > clock_timestamp())`. If this affects 0 rows, expiry won the race, so the Transfer is marked failed. First commit wins.
3. Post to the source and destination in `id` order with one atomic `UPDATE accounts SET posted = posted ± amount, version = version + 1 WHERE id = $1 RETURNING posted, version` each. The sign comes from the Account's normal balance, and there is **no version check**: the debit was reserved by the Hold at Accept, and a credit can't break never-negative. Concurrent captures on a hot Account queue on its row lock instead of conflicting and retrying. Then insert the debit and credit Entries with `balance_after`, `account_version` = new version, and `created_at = clock_timestamp()`.
4. Set the Transfer to `posted`. Commit.

**Batched capture (Rung 3 stage 2).** The worker runs steps 1-4 for up to `WORKER_BATCH` (default 100) of the oldest capturable Holds in **one tx**. It takes whatever is capturable right now with no waiting, so at low load a batch is a single Hold. Holds whose expiry won step 2 fail individually inside the batch. Postings are netted per Account: each Account row is updated once (`posted ± net, version = version + k` for its k legs, in ascending `id` order). Each of its Entries gets a consecutive `account_version` and a running `balance_after` derived from the returned totals, so invariants 2 and 4 and point-in-time reads are unchanged. A hot Account takes one row-lock hold and the batch makes one commit for many captures, which cuts both waits P4.3's diagnosis found (row lock 38%, WAL flush 20%).

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

**CAS counters.** Every CAS tx counts `<op>.attempts`, `<op>.conflicts` and `<op>.exhausted` (ops: `accept`, `capture`, `capture_hold`, `release`, `reverse`) in a stdlib `expvar` map named `cas`, with no new dependency. The api serves it at `/debug/vars`. Both processes log it every 10s and at shutdown, and `run.sh` prints the final totals. The map is created in `main`, and the ledger only sees a `store.Counter` interface, so the library keeps no global state.

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

## 9. Rung 4: sharding and the TigerBeetle comparison

Decided in P5.1. This section describes the Rung 4 design. §4-§6 stay authoritative for single-shard behavior.

### 9.1 Placement and routing
- **2 Postgres shards**, each a separate instance with the full §4 schema. On 2 shards, about 50% of random P2P pairs cross shards.
- **Shard bits inside UUIDv7.** The top 4 bits of `rand_a` (right after the version nibble) carry the shard number, which allows up to 16 shards and leaves time ordering untouched. `shard(id)` is a pure function of the id, so every process, the checker and the harness route without a lookup. Every id is minted with its shard bits:
  - **Wallets**: round-robin at creation.
  - **Funding**: one funding System account **per shard**, so TopUp and Withdraw are always single-shard.
  - **Receivable**: minted with **its debtor's shard bits**, so the Receivable rules stay single-shard.
  - **Transfer and Hold**: the source Account's shard.
  - **Entry**: its Account's shard.
- **Idempotency keys** live on shard `hash(key) mod N`. The key namespace stays global and mismatch detection exact (Decision Log → *Sharded idempotency keys join the 2PC*).
- **Non-goal: resharding.** N is fixed when ids are minted. A later deployment-scale experiment may revisit it.

### 9.2 Cross-shard writes: one generic transaction object
Every write path runs through a **cross-shard transaction** (`store.XTx`). It lazily begins a tx on each shard the path touches. On commit:
- **One shard touched:** a plain local commit. That's all single-shard behavior, unchanged.
- **Two or more:** 2PC. `PREPARE TRANSACTION` on each participant in shard order, then the decision row, then `COMMIT PREPARED` on each, then delete the decision (§9.4).

The **decision lives on the operation's home shard**: the shard of the Transfer the operation creates or changes. The gid embeds that Transfer's id, so the resolver finds the decision from the gid alone. Version conflicts retry the whole object, like `RunCAS`.

| Path | Participants | Home |
|---|---|---|
| CreateTransfer, PlaceHold, TopUp, Withdraw, Repay | key shard K = FNV-1a(key) mod N; source shard S | S |
| ReleaseHold | K; the Hold's shard | the Hold's shard |
| CaptureHold, Capture | K (CaptureHold only); source shard; destination shard | source shard |
| ReverseTransfer | K; payer shard (the original Transfer is locked there); recipient shard (new Transfers and the receivable) | recipient shard |

- **TopUp and Withdraw** use the funding account **on the Wallet's own shard**, so with K = S they stay local.
- **An in-flight duplicate** blocks on K's unique index until the original commits or rolls back, then replays.
- **Destination reads:** on another shard, the destination's existence and kind are read **outside** the tx from its own shard. Accounts are never deleted, so that read can't go stale.
- **Foreign keys that can cross shards are dropped**, and the cross-shard checker verifies those references instead (§9.6):
  - `idempotency_keys.transfer_id` (migration 00004): key and Transfer live on different shards.
  - `transfers.dest_id` and `holds.dest_id` (migration 00005): the destination can be on another shard.
  - `entries.transfer_id` (00005): a credit Entry lives on the destination's shard, while its Transfer is on the source's.
  - `transfers.reverses_id` (00005): a reversal lives on the recipient's shard, while the original is on the payer's.

  References that are always local keep their FKs: `source_id`, `entries.account_id`, a Hold's `transfer_id`, and a receivable's `debtor_wallet_id`.
- **Distributed deadlock:** each shard only sees its own waits, so a cycle that spans shards can't be detected by Postgres. Multi-shard sessions run with `lock_timeout = 5s`, and a timeout is treated as a conflict and retried.

### 9.3 Capture: 2PC by default
A Hold's debit lives on its source shard S and its credit on the destination's shard D. The worker claims batches **per source shard** (`FOR UPDATE SKIP LOCKED` on S, as §5), and posts each claimed batch as **one cross-shard write** (§9.2):
- **Participants:** S plus every destination shard the batch credits. A batch with only local destinations commits locally, exactly as §5. A batch with mixed destinations commits all of its Holds atomically in one 2PC. With 2 shards that's at most S → D.
- **Steps on S:** claim, mark captured, post the debits (`ApplyNet`), insert the debit Entries, set the Transfers to `posted`.
- **Steps on each D:** post the credits and insert the credit Entries. Accounts are posted in ascending id order, which is a global order because the shard bits sit below the timestamp.
- **Home:** the batch's first Transfer, on S. The gid is `x-<that id>`, and S holds the decision (§9.4).
- **Batching by destination shard was considered and rejected.** One write per claimed batch keeps the FIFO claim simple and nets each hot Account once per batch. The cost, with more than 2 shards, is more 2PC participants per mixed batch instead of more batches.

**Nothing bends:** debit and credit become visible atomically. The recipient was already credited asynchronously by capture on a single shard, so the sender's strict read-your-writes and never-negative are unchanged.

### 9.4 2PC recovery (decision log + resolver)
- **Each shard has a `decisions (gid PK, outcome, decided_at)` table.** `outcome` is `commit` or `abort`. The row on the write's home shard **arbitrates** the outcome. Both sides insert with `ON CONFLICT DO NOTHING`, and whichever row lands first decides:
  - **Coordinator:** after preparing every participant, it inserts `commit`. If an `abort` is already there, the resolver got in first: the coordinator rolls back its prepared txs and retries the whole write, and nothing was committed.
  - **Resolver** (a worker loop, every `RESOLVE_INTERVAL`, default 1s): it scans `pg_prepared_xacts` on every shard. For each gid it finds the home shard from the gid and reads the decision:
    - `commit`: `COMMIT PREPARED` on that shard.
    - `abort`, or none while the prepared tx is older than `PREPARE_TIMEOUT` (default 10s): it inserts `abort` (`ON CONFLICT DO NOTHING`), re-reads the outcome, and rolls back unless the row it reads says `commit`.
  - **Why:** presumed abort alone is unsafe. A slow but live coordinator could write `commit` after the resolver rolled back, leaving a split outcome. Arbitrating on the primary key makes the two outcomes mutually exclusive.
- **Cleanup:** the coordinator deletes its `commit` row after phase 2. The resolver deletes decisions older than `PREPARE_TIMEOUT` whose gid is no longer prepared on any shard.
- **Failpoints** for the Rung 2 matrix: `twopc.after_first_prepare`, `twopc.after_all_prepared`, `twopc.after_decision`, `twopc.after_first_commit`.
- **Counters** in the `cas` expvar map: `twopc.commits`, `twopc.aborts`, `resolve.committed`, `resolve.rolled_back`.
- Postgres needs `max_prepared_transactions` > 0 (P5.2).

### 9.5 Saga variant (comparison only, `CROSS_SHARD=saga`)
**Scope:** only the worker's **capture posting** switches. Key placement (Accept, Release, CaptureHold) and the Reversal lock stay on 2PC in both modes, because they need atomicity for exactly-once guarantees, not just for balance. So the comparison is "posting via 2PC vs via saga". Manual CaptureHold also keeps 2PC.

- **Leg 1, on S:** the capture batch posts every debit, posts the credits whose destination is also on S, and writes an `outbox (transfer_id PK, dest_id, amount)` row for each credit whose destination lives on another shard. All in one **local** tx, with no 2PC. The Transfer becomes `posted` here, when its debit lands.
- **Leg 2, on D:** a relay (a worker loop) claims outbox rows on S (`FOR UPDATE SKIP LOCKED`) and, per destination shard, applies the credits in one local tx through the same netted posting as §5. It skips any Transfer already credited there, with a unique `(transfer_id, direction)` on Entries as the backstop. Only then does it delete the claimed outbox rows on S. A crash between the two (failpoint `saga.after_credit`) leaves the outbox row, and the next relay finds the credit already applied and just deletes it. Each credit is applied exactly once.
- **Relaxations, documented and measured:** the recipient is credited after the sender is debited, and the ledger is globally unbalanced by the outbox total while legs are in flight.
  - The **checker** counts outbox rows as in-flight credits: a Transfer with an outbox row is expected to net its amount, and the ledger to net Σ outbox.
  - The **harness drain** waits for the outbox to empty, so post-drain checks are strict.
- **Counters:** `saga.outbox_written`, `saga.relayed`, `saga.already_applied`. The default stays 2PC.

### 9.6 Reads and the checker
- **Routing:** GetTransfer, GetBalance and GetBalanceAt route by id. ListEntries routes by account. ListReceivables fans out to every shard and merges by debtor id.
- **Checker:** two kinds of check.
  - **Local:** SQL run on each shard. Invariants 2, 3 and 4; invariant 5's Hold half; receivables-never-overpaid from 7; and invariant 8's "no prepared tx".
  - **Global:** checks whose rows span shards. The ledger and per-Transfer balance (1), "posted ⇔ has Entries" (5), keys → Transfers (6), reversal totals (7), and every cross-shard reference (8). These are computed in Go from a compact projection read off every shard.

  Each shard is read in its own read-only REPEATABLE READ snapshot. Together they're consistent only while the ledger is idle, so the checker runs after the drain, as the harness already does. With one shard, every check means exactly what it did before.

### 9.7 TigerBeetle backend (`LEDGER_ENGINE=tigerbeetle`)
The same API runs on a single-replica TigerBeetle, which isn't sharded: the comparison is **sharded Postgres vs one TigerBeetle**. It has full API parity using native mechanisms only, with the two gaps proven atomic by `prototypes/tb-gaps` (Decision Log → *TigerBeetle gaps close with linked chains*).

| Ledger rule | TigerBeetle mechanism |
|---|---|
| Hold / Capture (partial, remainder restored) / Release | `pending` / `post_pending_transfer` / `void_pending_transfer` |
| Expiry, never-expiring system Holds | `timeout` in **whole seconds**, so TTLs round up. `timeout = 0` meaning "never expires" is verified in P5.3 |
| Wallet never negative (Available) | `debits_must_not_exceed_credits` (pending debits included) |
| Idempotency | Transfer id = a deterministic 128-bit hash of the Idempotency-Key. A mismatch shows as `exists_with_different_*` |
| No overpaid Receivable | `credits_must_not_exceed_debits` on the receivable account |
| Open Receivable blocks debits | Each non-repayment Wallet debit is linked to a `balancing_credit` from a zero-balance control account into the debtor's receivable. The chain fails if anything is owed |
| Reversal shortfall | Linked chain via a control account: C→A X, B→C `balancing_debit` X (moves r), R→C `balancing_credit` X (moves X − r) |
| Point-in-time balance, statements | `history` flag + `get_account_balances`, `get_account_transfers` |
| Receivables list | Query the receivable accounts and filter owed > 0 in the app |

**Posting model (P5.10 decision): single-phase where possible.**
- **Posted at once:** CreateTransfer, TopUp, Withdraw, Repay and Reversal post as **one atomic TigerBeetle transfer**, with limits enforced in the engine. The API returns `POSTED`, and there's no capture worker or backlog.
- **Two-phase only for Holds:** PlaceHold, CaptureHold and ReleaseHold use `pending` / `post_pending_transfer` / `void_pending_transfer`.
- **Idempotency is the transfer id:** a deterministic 128-bit hash of the Idempotency-Key. Same fields replay; different fields come back as `exists_with_different_*` and map to `IDEMPOTENCY_MISMATCH`. Keys never expire, where Postgres keeps them 24h.
- **Capture and Release use ids derived from the Hold id,** so TigerBeetle can tell a Hold's status without its own store. **Parity limit:** capturing or releasing an already-settled Hold with a *new* key returns its current state, where Postgres returns `HOLD_NOT_ACTIVE`. This is recorded as a finding.
- **Build:** the engine compiles only with `-tags tigerbeetle`. A `tigerbeetle` Dockerfile target (cgo, `gcr.io/distroless/base-debian13` to match the build image's glibc 2.41) produces `payment-ledger:tb`, and the TigerBeetle overlay uses it. The default image stays static.

**Constraints found by the prototype:**
- The Go client's macOS native library fails to link, so TigerBeetle code builds and tests **only in Linux containers**.
- The client needs **io_uring**, so its containers need `seccomp=unconfined`, the same as the server.
- The data file needs a **Docker named volume**, because a macOS bind mount refuses its preallocation.
- Client 0.17.9 is pinned to server image 0.17.9.
- The client accepts only an IP (or a bare port), not a hostname, so the engine resolves Compose service names itself (P5.10).

**Parity findings (P5.10):**
- **Failed ids are permanent.** TigerBeetle remembers a transfer id that failed for a transient reason (e.g. `exceeds_credits`) and answers a later retry with `id_already_failed`. Because the id derives from the Idempotency-Key, a client that retries the same key after topping up can never succeed and must use a new key. The engine reports it as `INVALID_REQUEST`. In Postgres a rejected Accept rolls back its key, so a later retry can succeed.
- **Settling a Hold twice:** see the capture/release note above.

**Receivable rules (P5.11), native chains from `prototypes/tb-gaps`:**
- **Each Wallet is created with its receivable account** (code receivable, `credits_must_not_exceed_debits`, `user_data_128` = debtor), so the guard below always has a target.
- **Guarded debits.** Every Wallet debit except a Repayment (P2P, Withdraw, PlaceHold) is linked with a `balancing_credit` from a zero-balance control account D into the debtor's receivable. The chain fails when anything is owed, which maps to `RECEIVABLE_OPEN`.
- **Repay** credits the receivable, and an overpayment is refused by TigerBeetle itself.
- **ReverseTransfer** is one linked chain through a control account C: C→payer X; recipient→C `balancing_debit` X (moves r); receivable→C `balancing_credit` X (moves X − r). C nets to zero after every reversal, so one shared C works. The engine presents the result as Postgres does: a reversal (recipient → payer, r) and a receivable (receivable → payer, X − r), with `reverses_id` stored in `user_data_128`. **Parity limit:** the chain's ids derive from the original Transfer, so TigerBeetle enforces "reverse at most once" itself, but a second reversal with a new key replays the first, where Postgres returns `TRANSFER_NOT_REVERSIBLE`.
- **ListReceivables** queries receivable accounts and keeps those owing. `opened_at` is when the current run of non-zero balance began, from the account's balance history.

### 9.8 Rung 4 measurements (P5.4)
The Rung 1 baseline, Rung 3 hot runs and Rung 2 fault matrix are run on: sharded Postgres with 2PC, sharded Postgres with the saga, and TigerBeetle. Results are reported as ratios, since everything shares one machine. Cross-shard share and 2PC counts come from new expvar counters (`twopc.*`, `saga.*`).

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

### Rung 3: version checks only where a funds check needs them
P4.1 showed optimistic CAS everywhere turning hot Accounts into retry storms. With 80% of P2P going to one merchant, 51% of capture attempts conflicted (2,979 exhausted) and posted/s fell to 1,403. With 20% TopUps, 40% of accepts conflicted on the funding row and p99 hit 17 ms. A version check only earns its cost where it guards a funds check, which is Accept on a Wallet source. So capture posts with an atomic `posted ± x` under the row lock, and Accept no longer bumps System-account sources. The plan is staged by measurement: (1) these two changes; (2) batch captures per hot Account, bumping the version by n so each Entry keeps a consecutive version and its own `balance_after`, only if stage 1 misses the target; (3) sub-accounts for hot keys only if a batched single row still can't keep up, deciding then which Accounts may be split.

### Rung 4: 2PC by default, the saga as a measured variant
The user chose "nothing bends" for cross-shard Transfers, which only 2PC delivers: debit and credit commit atomically. A saga necessarily credits the recipient late and leaves the ledger unbalanced while legs are in flight. It's built anyway, behind `CROSS_SHARD=saga`, so its cost and relaxations can be measured against 2PC. 2PC's failure mode (prepared txs holding locks after a coordinator crash) is covered by a decision log plus a presumed-abort resolver.

### Shard bits inside UUIDv7
Each id carries its shard in the top 4 bits of `rand_a`, so routing is a pure function of the id. That gives co-location without a lookup service: a receivable takes its debtor's bits, and each shard has its own funding account. The cost is that N can't change without rewriting ids, so resharding is an explicit non-goal for Rung 4. A directory table was rejected (a lookup path and a single point of failure), and so was `hash(id)` plus special cases (System accounts scatter, and a new N reshuffles everything).

### Sharded idempotency keys join the 2PC
Keys live on shard `hash(key)`, not on the source account's shard. That keeps one global key namespace with exact `IDEMPOTENCY_MISMATCH` detection: a key reused for a different-shard source is still caught. The cost is a 2PC in Accept whenever the key's shard and the source shard differ (about 50% at 2 shards). Storing the key with the source would keep Accept single-shard but silently create a second Transfer on cross-shard key reuse.

### TigerBeetle gaps close with linked chains
Question: can the two rules without a native TigerBeetle flag be enforced atomically, or would app-side checks race (Rung 1 again)? Prototype `prototypes/tb-gaps` passed 21/21 checks against a real single-replica TigerBeetle 0.17.9.
1. **Open Receivable blocks debits:** link the debit with a `balancing_credit` from a zero-balance control account (`debits_must_not_exceed_credits`) into the receivable. It moves exactly what's owed, so the chain fails when anything is owed and is untouched otherwise.
2. **Reversal shortfall:** C→A X, B→C `balancing_debit` X, R→C `balancing_credit` X, linked. That returns r = min(X, B's available) and records the X − r shortfall in one atomic chain. Zero-amount balancing results are accepted.

Verdict: TigerBeetle parity uses native mechanisms only, with no app-side races.

### System-initiated Holds never expire
Every other Hold expires, but Reversal and receivable Holds have no expiry. If the worker is down or slow, a correction must not silently fail and leave a mistaken Transfer uncorrected. The cost is that the debtor's funds stay held until the worker recovers.
