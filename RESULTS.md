# payment-ledger: results

A double-entry payment ledger in Go, built rung by rung so that each rung exposes one real failure under load and then fixes it. The four rungs are a concurrent double-spend, a crash mid-Transfer, a hot Account, and sharding with cross-shard Transfers. Money moves in two steps. Accept reserves funds by placing a Hold on the source Wallet, in one Postgres transaction that also commits the Idempotency-Key. A separate worker then captures the Hold into balanced Entries. Every Account change goes through a version check (CAS) or a row lock, and an independent checker verifies the ledger invariants (A§4) after every run. For Rung 4 the same API runs on three backends: one Postgres shard, two Postgres shards with cross-shard writes by two-phase commit (2PC) or a saga, and TigerBeetle behind the same `api.Engine` interface. Design: [ARCHITECTURE.md](ARCHITECTURE.md). Requirements: [README.md](README.md). Per-rung detail: [retros/](retros/).

## Algorithm/Design Comparison
| Backend | Cross-shard write | Atomicity of debit + credit | Crash recovery | Capacity (ratio) | Best fit |
|---|---|---|---|---|---|
| Postgres, 1 shard | none needed | one local tx | tx rollback; worker re-claims via SKIP LOCKED | 4,000/s (1.0×) | Anything that fits one primary; the simplest correct design |
| Postgres, 2 shards, **2PC** (default) | PREPARE on each shard → decision row → COMMIT PREPARED | Atomic ("nothing bends") | Resolver: commit if the decision is commit, presume abort after 2s | 2,000/s (0.5×) | Strict cross-shard atomicity across separate machines |
| Postgres, 2 shards, **saga** | Debit locally + outbox row; a relay applies the credit | Recipient credited late; ledger unbalanced while in flight | Relay re-applies idempotently (unique transfer_id + direction) | 3,000/s (0.75×) | Throughput over immediate credit visibility |
| **TigerBeetle** 0.17.9 | n/a (single cluster) | Native linked chains | Replica restarts from its data file | ≥ 5,000/s (≥ 1.25×, not saturated) | High-throughput ledgers that fit TigerBeetle's account model |

Ratios are capacities on one machine, where both shards share its CPU. They measure each protocol's coordination cost, not scale-out.

## Performance Results
**Test environment:** MacBook Pro M4 Pro, macOS (Darwin). Docker Desktop VM with 14 CPUs, where every service runs: Postgres 18, TigerBeetle 0.17.9 (single replica, `--development`), api and worker (Go 1.27.1, connect-go over h2c), and k6 2.3.0 over gRPC. Numbers are relative, since the VM and a shared host add overhead. Rung 4 was measured on 2026-10-02/03; Rungs 1–3 dates are in their retros. Each configuration ran once, with 60s cool-downs between runs. VU ranges are k6 arrival-rate pools (pre-allocated–max), not fixed connections.

### Correctness Tests
**Test suites** (run 2026-10-03 on merged main, `go test -count=1 -p 1 -race`):

| Mode | Env | Pass | Skip | Fail |
|---|---|---|---|---|
| Two shards | `DATABASE_URL` + `SHARD_URLS` (2) | 65 | 0 | 0 |
| One shard | `DATABASE_URL` + `SHARD_URLS` (1) | 48 | 17 (two-shard only) | 0 |
| TigerBeetle (`-tags tigerbeetle`, Linux container) | `TB_ADDRESS` | 5 | 0 | 0 (last run at P5.11) |

**Concurrency, atomicity and recovery tests:**

| Test | Input | Expected | Actual | Result |
|---|---|---|---|---|
| `TestConcurrentAcceptsCannotOverspend` | 20 racers from a start barrier, each sending 60 of 100, ×10 rounds | exactly 1 winner per round | naive Accept fails 10/10; CAS fix passes 10/10 under `-race` | PASS |
| `TestCrossShardAcceptsCannotOverspend` | racers whose keys hash to different shards | no overspend | no overspend | PASS |
| `TestRunCASConcurrentWritersLoseNothing` | concurrent CAS writers on one row | every increment lands | every increment lands | PASS |
| `TestHotDestinationCapturesDoNotConflict` | captures into one merchant Wallet | 0 CAS conflicts | 0 | PASS |
| `TestCrashDuring{Accept,Capture,Release,Reversal,Sweep}` | failpoint before/after commit, mid-batch | before: nothing persists; after: a same-key retry returns the result | as expected | PASS |
| `TestResolverFinishesCrashedCoordinators` | coordinator crash at each `twopc.*` point | rolled back before the decision, committed after; nothing left over | as expected | PASS |
| `TestResolverAbortBeatsSlowCoordinator` | resolver aborts while the coordinator is still live | the coordinator sees `abort` and retries; no split outcome | as expected | PASS |
| `TestXTxPoolsCannotDeadlock` | 16 writes taking the shards in opposite orders, 2-connection pools | all commit, no leftovers | without the cap: deadline errors + stranded prepares; with it: all commit | PASS |
| `TestCrossShardReplays` | same-key retry of ReleaseHold / CaptureHold / ReverseTransfer, key on another shard | the replay returns the original result | without the fix: "no home Transfer" on attempt 2; with it: returns | PASS |
| `TestSagaRelayCrashAfterCreditAppliesOnce` | relay crash after the credit, before the outbox delete | the credit applies once | once | PASS |
| `TestChecksDetectCorruption` | hand-corrupted rows, one per invariant | each check reports it | each reports | PASS |

**Harness runs.** The checker plus the acknowledged-Transfer check ran after every load run:

| Rung | Runs | Faults | Lost / duplicated money | Acknowledged Transfers not posted once | Checker |
|---|---|---|---|---|---|
| 1 | naive vs fixed Accept | none (race) | naive: 5/5 senders overdrawn, 12 negative Entries; fixed: 0 | n/a | naive FAIL, fixed CLEAN |
| 2 | 11-run matrix | 10 failpoints + kill -9 api/worker/Postgres | 0 | 0 of 48,025 (reference run) | CLEAN ×11 |
| 3 | 3 hot runs ×3 stages + Rung 2 matrix rerun | 27 crashes + 6 kills | 0 | 0 | CLEAN everywhere |
| 4 | 7 throughput + 5 saturation + 10 fault runs | 4 `twopc.*` + saga failpoint, kill -9 incl. both shards and TigerBeetle, toxiproxy partitions | 0 | 0 | CLEAN ×22 |

### Latency
Rung 4 Batch A: offered 2,000/s for 30s, 100 Wallets, gRPC request duration. The template's p2.5/p97.5 columns weren't recorded: k6 was configured for avg, p50, p95, p99 and max.

| Backend / scenario | p50 | p95 | p99 | avg | max |
|---|---|---|---|---|---|
| 1 shard, uniform P2P | 0.86 ms | 2.35 ms | 11.32 ms | 1.25 ms | 59.6 ms |
| 2 shards 2PC, uniform | 3.29 ms | 8.97 ms | 34.81 ms | 4.22 ms | 323.4 ms |
| 2 shards saga, uniform | 2.42 ms | 4.89 ms | 8.20 ms | 2.61 ms | 35.1 ms |
| TigerBeetle, uniform | 1.53 ms | 12.47 ms | 26.85 ms | 3.37 ms | 133.1 ms |
| 2 shards 2PC, hot Accounts | 2.67 ms | 5.20 ms | 10.66 ms | 2.74 ms | 81.9 ms |
| 2 shards saga, hot Accounts | 2.09 ms | 4.32 ms | 7.76 ms | 2.34 ms | 58.2 ms |
| TigerBeetle, hot Accounts | 1.44 ms | 9.43 ms | 17.52 ms | 2.52 ms | 57.9 ms |

Earlier rungs, one shard, measured without faults: Rung 1 baseline p99 **3.78 ms** (6.27 ms on a later re-measure). Rung 3 hot Accounts after batched capture p99 **4.83–5.57 ms**.

### Throughput
| Scenario | Connections (VUs) | Duration | Total requests | Avg RPS | Responses | Data read | Target assessment |
|---|---|---|---|---|---|---|---|
| R1 baseline, 1 shard | 100–400 | 30s | ≈60,000 | 2,000 posted/s | all accepted, 0 backlog | — | Sets the baseline: 2,000 posted/s at p99 3.78 ms |
| R2 fault-free mix, 1 shard | 200–1,000 | 40s | ≈80,000 | 1,945–1,987 ops/s | 0 failed, 0 gave up | — | Throughput pass; p99 8.67 ms missed the 7.6 ms gate (accepted with a note) |
| R3 hot Accounts (both), batched capture | 200–1,000 | 30s | 60,000 | 1,600 P2P posted/s of 2,000 ops/s | 0 failed | — | Pass, p99 4.83 ms |
| R4 A1: 1 shard | 100–400 | 30s | 60,002 | 2,001 posted/s | 0 dropped | 11.4 MB | Keeps up |
| R4 A2: 2PC | 100–400 | 30s | 59,932 | 1,998 posted/s | 69 dropped | 11.5 MB | Keeps up |
| R4 A3: saga | 100–400 | 30s | 60,000 | 2,000 posted/s | 0 dropped | 11.5 MB | Keeps up |
| R4 A4: TigerBeetle | 100–400 | 30s | 59,778 | 1,983/s (accept = post) | 223 dropped | 11.9 MB | Keeps up |
| R4 saturation, 1 shard | up to 1,000 | 50s (2k→6k/s) | — | capacity 4,000/s, max 4,720/s | 192 CAS-exhausted at overload | — | 1.0× |
| R4 saturation, 2PC | up to 1,000 | 50s | — | capacity 2,000/s, max 2,589/s | 111 CAS-exhausted | — | **0.5×** |
| R4 saturation, saga | up to 1,000 | 50s | — | capacity 3,000/s, max 3,493/s | 0 failed | — | **0.75×** |
| R4 saturation, TigerBeetle | up to 1,000 | 50s | — | ≥ 5,000/s, max 5,934/s | 0.9–1.1% dropped at 5–6k | — | **≥ 1.25×** |

"—" means the value wasn't recorded in that rung's retro.

### Observations
1. **The double-spend was invisible to an end-state check.** In Rung 1 Run A the race happened (2 negative Entries, worst −43), but later credits refilled the Wallet and the checker reported CLEAN. Only a history invariant (`balance_after < 0` on any Entry) and a one-way workload made it deterministic: 5/5 senders overdrawn. The CAS fix cost nothing measurable (p99 6.20 → 6.21 ms).
2. **Requests per second hid two bottlenecks.** Rung 1's 16,885 req/s was almost all rejections. Measuring posted/s end to end exposed capture collapsing to 566/s at an offered 4,000/s, because the claim query walked 149k settled Holds at 28 ms per claim. A partial index restored 2,000 posted/s.
3. **Crash safety came from structure, not recovery code.** One tx per write path, with the Idempotency-Key in the same tx, gave 0 lost and 0 duplicated money across 10 failpoints and kill -9s in Rung 2, without a line of crash-specific recovery logic.
4. **Optimistic CAS turned hot rows into retry storms.** 51% of capture attempts conflicted on a hot merchant. Version checks now apply only where they guard a funds check. Captures lock the row instead, and batching nets each Account once per batch, which took capture conflicts from 51% to 0 and p99 from 17.0 to 4.83 ms.
5. **Sharding on one machine halves capacity under 2PC.** About half of all Accepts become 2PC because the key's shard differs from the source's: A2 logged 29,842 api-side 2PC commits for about 60,000 Accepts. Doubling the admission slots raised 2PC's ceiling only 12%, so the protocol, not the cap, is the limit.
6. **TigerBeetle is fastest at the median but not at the tail.** Its p50 is the lowest of the multi-shard-capable backends (1.44–1.53 ms), but its p95/p99 at 2,000/s are 2–3× the saga's (9.4–12.5 ms and 17.5–26.9 ms). Past saturation it degrades far more gently: p99 stayed under 52 ms at 6,000/s, while Postgres reached 0.5–1.5 s. The tail cause is not yet diagnosed.
7. **The checker can't see availability bugs.** Three of the four Rung 4 bugs left the ledger CLEAN: the pool deadlock (121 ops/s at a 5s p99), stranded prepares (238 give-ups per run), and failing cross-shard replays. Only client-side signals (give-ups, deadline errors, the last status before giving up) exposed them.
8. **Single-run p99 isn't reliable on a laptop.** The same one-shard run measured 4.3 ms and 11.3 ms an hour apart, and Rung 2's fault-free p99 swung 8.67 → 16.64 ms. Capacity ratios are stable at this size; individual p99 values are rough indicators.

## Decision Log walkthrough
Each entry in [ARCHITECTURE.md → Decision Log](ARCHITECTURE.md#decision-log), with the evidence from its rung.

| Decision | Rung | Evidence |
|---|---|---|
| **Hand-built Postgres ledger before TigerBeetle** | 1–4 | Building it by hand surfaced every lesson the rungs were designed to teach: the double-spend (R1), capture collapsing on an unindexed claim (R1), crash safety from structure (R2), retry storms on hot rows (R3), and three distributed-coordination bugs (R4). TigerBeetle then reached ≥ 1.25× capacity and needed none of the 2PC machinery. |
| **Rung 1 ships an intentionally racy Accept** | 1 | Naive Accept: 5/5 senders overdrawn, 12 negative Entries, checker FAIL. Its regression test fails 10/10 against the naive code. The intentional race is what proved the test can catch it. |
| **Placing a Hold bumps the Account version** | 1 | With the CAS fix and the same load, the checker went CLEAN with 0 retries exhausted and p99 6.21 ms against 6.20. `TestConcurrentAcceptsCannotOverspend` and its cross-shard twin pass. |
| **Receivable creation bumps the debtor Wallet version** | 2 | Covered by `TestReceivableBlocksDebitsUntilRepaid` and `TestReversal*`. Reversals were 10% of every Rung 2 and Rung 4 fault run, with invariant 7 (receivables never overpaid) CLEAN throughout. No load run targeted this specific race, so the evidence comes from tests and invariants, not an observed collision. |
| **CAS before Entries, version as tiebreaker** | 1–3 | Invariant 4 (Entry versions strictly increase per Account) held in every run, including batched capture, which bumps the version by k and gives each Entry a consecutive version. `TestGetBalanceAt` covers point-in-time reads. |
| **Rung gates: total ops/s, and p99 only without faults** | 2 | With an 80% P2P mix, P2P posted/s maxes out near 1,600 at 2,000 offered. A crash always delays in-flight calls: p99 was 22–49 ms on api crashes and 1.07 s on Postgres restarts. |
| **Rung 3: version checks only where a funds check needs them** | 3 | Capture conflicts 51% → 0. Accept conflicts on the funding row 40% → 3%. p99 17.0 → 4.62 ms. Batching (stage 2) then removed the row-lock queue: 79 → 1 wait samples. |
| **Rung 4: 2PC by default, the saga as a measured variant** | 4 | Measured cost: 2PC 0.5×, saga 0.75× of one-shard capacity. Both kept every invariant through crashes and partitions. The saga's relaxation shows up in the checker's in-flight allowance (`TestInFlightSagaCreditIsNotAViolation`). |
| **Shard bits inside UUIDv7** | 4 | Routing is a pure function of the id (`TestNewIDCarriesShard`, `TestTimeOrderPreserved`), and the cross-shard reference check (invariant 8) was CLEAN in every run. The stated cost (no resharding) wasn't exercised. |
| **Sharded idempotency keys join the 2PC** | 4 | It costs about 50% of Accepts as 2PC (29,842 of ≈60,000 in A2), the main reason for 2PC's 0.5×. The cross-shard replay bug also lived on this path. |
| **TigerBeetle gaps close with linked chains** | 4 | Prototype passed 21/21. `TestReversalAndReceivableRules` passes. The TigerBeetle audit, including receivables never overpaid and control accounts at zero, was CLEAN in every TigerBeetle run, including kill -9. |
| **System-initiated Holds never expire** | 2 | Worker kill -9 runs (Rung 2, Rung 4 B6/B7) left no uncorrected reversal and stayed CLEAN. A long worker outage, the case this rule protects, wasn't load-tested. |
| **2PC admission cap instead of ordered acquisition** | 4 | 121 → 2,000 posted/s on the same run, with p99 from 5 s to 14.5 ms. Pool size alone moved the failure point: 4 connections collapsed, 14 collapsed over 30s, 64 ran clean. Covered by `TestXTxPoolsCannotDeadlock`. |
| **2PC prepare timeout: 2s, not 10s** | 4 | Same failpoint run: give-ups 238 → 1, ops/s 800 → 1,532. On the final code, all 10 fault runs had 0 give-ups. |

## Screenshots
<!-- Add screenshot: harness/run.sh "== result" block from a CLEAN Rung 4 2PC fault run (B2), showing k6 exit 0 and the checker passing with the acknowledged-transfers line -->
<!-- Add screenshot: Rung 1 naive run checker output showing "FAIL invariant 3" with negative-balance samples -->
<!-- Add screenshot: k6 per-plateau saturation summary for TigerBeetle vs 2PC (offered vs achieved, dropped, p99) -->
<!-- Add screenshot: Docker Desktop resource graph during a 2,000/s two-shard run -->
<!-- Add screenshot: go tool buf curl CreateTransfer then GetBalance response against the running api -->
