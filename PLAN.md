# payment-ledger — Implementation Plan

This plan covers task order, verification, and commit sequence. Requirements are in [README.md](./README.md), terms in [CONTEXT.md](./CONTEXT.md), and design in [ARCHITECTURE.md](./ARCHITECTURE.md) (cited as A§n). Nothing from those files is restated here.

## Implementation Checklist

### Phase 0 — Scaffolding (§P0)
- [x] P0.1 `git init`, `main` + branch `p0-scaffold`, `.gitignore` (ask-first) → §P0
- [x] P0.2 `go.mod` (module `payment-ledger`), Go toolchain version recorded → §P0
- [x] P0.3 `deploy/docker-compose.yml`: Postgres service (ask-first) → §P0
- [x] P0.4 goose migrations for the A§4 schema, embedded + `migrate` subcommand → §P0
- [x] P0.5 `proto/ledger/v1/ledger.proto` for all A§7 RPCs + buf codegen → §P0
- [x] P0.6 `cmd/api` skeleton: connect-go over h2c, auth interceptor, error mapping (A§7 table) → §P0
- [x] P0.7 `cmd/seed`: create funding System account + N Wallets → §P0

### Phase 1 — Rung 1: lost update / double-spend (§P1)
- [ ] P1.1 Store layer: tx helper, CAS helper with bounded retry, ascending-id row ordering (A§5) → §P1
- [ ] P1.2 Idempotency insert/match/mismatch/in-flight wait (A§5 Accept step 1) → §P1
- [ ] P1.3 **Naive** Accept for CreateTransfer + TopUp (no CAS; see A Decision Log) → §P1
- [ ] P1.4 `cmd/worker` Capture loop with SKIP LOCKED + Entries (direction, balance_after, account_version) → §P1
- [ ] P1.5 Reads: GetTransfer, GetBalance (Available derived per A§4) → §P1
- [ ] P1.6 `cmd/checker`: invariants 1-6 as SQL, `--once` / `--interval`, non-zero exit on violation → §P1
- [ ] P1.7 `harness/k6/rung1.js` + `harness/run.sh <rung>`: one-command up → migrate → seed → load → check → report → §P1
- [ ] P1.8 Run naive: capture the double-spend (checker fails invariant 3) + record baseline TPS/p99 → §P1
- [ ] P1.9 Fix: version CAS in Accept (Hold bumps version); rerun until clean → §P1
- [ ] P1.10 `retros/rung-1.md` + set TPS targets for Rungs 2-4 (update README table) → §P1

### Phase 2 — Feature completion (§P2)
- [ ] P2.1 PlaceHold / CaptureHold (partial, remainder released) / ReleaseHold → §P2
- [ ] P2.2 Lazy expiry in funds checks + sweeper loop + idempotency purge (24h) → §P2
- [ ] P2.3 Withdraw → §P2
- [ ] P2.4 ReverseTransfer: two Transfers, receivable System account, non-expiring Holds, debtor version bump → §P2
- [ ] P2.5 Repay + RECEIVABLE_OPEN block → §P2
- [ ] P2.6 GetBalanceAt, ListEntries (cursor), ListReceivables → §P2
- [ ] P2.7 Checker + k6 scenarios cover Holds, Reversals, Repayments → §P2

### Phase 3 — Rung 2: crash mid-Transfer (§P3)
- [ ] P3.1 `internal/failpoint`: env-gated named crash points → §P3
- [ ] P3.2 Wire all 10 failpoints from A§5 → §P3
- [ ] P3.3 `harness/faults/`: per-failpoint runs + random kill -9 of api/worker/postgres under load → §P3
- [ ] P3.4 k6 client retries with the same Idempotency-Key on errors/timeouts → §P3
- [ ] P3.5 Run to target; fix findings; `retros/rung-2.md` → §P3

### Phase 4 — Rung 3: hot Account contention (§P4)
- [ ] P4.1 `harness/k6/rung3.js`: skewed traffic on funding account + popular destination; record retries + p99 → §P4
- [>] P4.2 Hot-Account strategy (design + Decision Log entry) → defer until: P4.1 measurements exist
- [>] P4.3 Implement strategy, rerun to target, `retros/rung-3.md` → defer until: P4.2 approved

### Phase 5 — Rung 4: sharding + cross-shard (§P5)
- [>] P5.1 Shard key + cross-shard protocol + strict-reads vs never-negative decision (ARCH update) → defer until: Rung 3 passes
- [>] P5.2 Multi-Postgres Compose + toxiproxy → defer until: P5.1 approved
- [>] P5.3 TigerBeetle comparison implementation → defer until: P5.1 approved
- [>] P5.4 Run both, compare, `retros/rung-4.md` → defer until: P5.2 + P5.3 done

### Phase 6 — Writeup (§P6)
- [>] P6.1 Interview writeup via `/docs` (design, rung results, Decision Log walkthrough) → defer until: Rung 4 passes

---

## Project structure

```
payment-ledger/
├── README.md  CONTEXT.md  ARCHITECTURE.md  PLAN.md  CLAUDE.md
├── go.mod
├── buf.yaml  buf.gen.yaml
├── proto/ledger/v1/ledger.proto
├── gen/ledger/v1/              # buf output, committed
├── cmd/
│   ├── api/                    # connect-go server
│   ├── worker/                 # capture + sweeper/purge loops
│   ├── checker/                # invariant checker
│   └── seed/
├── internal/
│   ├── ledger/                 # write paths + reads (A§5)
│   ├── store/                  # pgx, tx + CAS helpers, queries
│   ├── api/                    # handlers, auth interceptor, error mapping
│   ├── failpoint/
│   └── checker/                # invariant SQL
├── migrations/                 # goose SQL, embedded
├── deploy/docker-compose.yml
├── harness/
│   ├── run.sh                  # one command per rung
│   ├── k6/
│   └── faults/
└── retros/
```

## Dependency handling
Every dep is from A§3 and has already been agreed. At its first-use task, add it with `go get <pkg>@latest` (or the tool's install equivalent), then record the resolved version in the commit message. Existing versions are never bumped. Tasks that add deps: P0.4 goose + pgx (goose needs its database/sql adapter), P0.5 connect/protobuf + buf and protoc plugins as go.mod `tool` deps, P0.7 uuid (seed needs UUIDv7). k6 runs from its Docker image.

## §P0 — Scaffolding
Covers repo, module, Postgres in Compose, schema, proto contract, and the API shell with auth and errors.
**Verify:** `docker compose up` gives a healthy Postgres. `migrate` applies all migrations cleanly on a fresh DB, and re-running is a no-op. `buf generate` output compiles. A curl to Connect HTTP/JSON and a grpcurl/k6 plaintext gRPC call both reach the API. A missing token returns 401 with the A§7 message. An unimplemented RPC returns `UNIMPLEMENTED`.

## §P1 — Rung 1
Covers enough of the ledger to move money (TopUp + P2P), run it naively, show it breaking, and fix it.
**Verify:**
- Unit tests: CAS retry and ordering helper; idempotency match, mismatch, and in-flight wait.
- `harness/run.sh 1` on the naive build: the checker reports an invariant 3 violation, and the baseline TPS and p99 are recorded.
- After the fix, `run.sh 1` at the baseline load: the checker is clean. Then fill in the README Rung table.

## §P2 — Feature completion
Covers every remaining A§7 RPC and A§5 path.
**Verify:**
- Integration tests against Compose Postgres for each path: partial capture releases the remainder; an expired Hold is excluded from Available before the sweep; a Capture/expiry race lets only one side win; a Reversal with a partly spent balance creates a Receivable and blocks debits except Repay; GetBalanceAt matches a hand-computed history.
- `run.sh 1` stays clean.

## §P3 — Rung 2
**Verify:** for each of the 10 failpoints plus random kill -9 under load, the checker is clean after recovery, no Transfer is lost or duplicated, and client retries converge to one Transfer per key. The TPS target set in P1.10 is met.

## §P4 — Rung 3
**Verify:** meet the target at the skewed load with a clean checker. The chosen strategy is recorded in the Decision Log.

## §P5 — Rung 4
**Verify:** meet the target on the sharded setup for both implementations. The checker is clean across shards, including under toxiproxy partitions. The comparison goes in the retro.

## §P6 — Writeup
**Verify:** each Decision Log entry is walked through with the evidence from its rung.

## Commit sequence
Each phase gets a feature branch (`p0-scaffold`, `p1-rung1`, `p2-features`, `p3-rung2`, …). There's one commit per checklist item, with a message of the form `P1.3: naive accept path`. P1.8 and P1.9 are separate commits, so the racy version stays in history on purpose. Branches merge to `main` only after that phase's verification passes. Commits and merges happen only when you ask.
