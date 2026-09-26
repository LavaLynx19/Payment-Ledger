# Rung 1 retro: lost update / double-spend

Status: **done**. Rung 1 passes: naive double-spend captured, CAS fix verified, baseline and targets set.

Environment: MacBook M4 Pro, Docker Compose (VM overhead included, so numbers are relative). Postgres 18, 1 api container, 1 worker container (4 capture loops), k6 2.3.0 over gRPC/h2c, 50 VUs × 30s.

## Naive Accept: what happened

### Run A: circulating money (10 Wallets, all send and receive)
| Metric | Value |
|---|---|
| Requests | 463,961 (15,348 req/s) |
| Accepted / rejected | 37,175 (1,230/s) / 426,786 |
| gRPC p50 / p95 / p99 | 2.93 / 5.03 / 6.66 ms |
| Checker (current state) | **CLEAN** |
| Negative Entries in history (manual query) | 2, on 1 Wallet, worst −43 |

The race happened, but incoming credits refilled the overspent Wallet before the run ended, and the checker only looked at final balances. The bug was invisible to the end-state check.

Two harness gaps came out of this:
1. **Invariant 3 has to hold over history, not just the current state.** A§4 was updated, and the checker gained "wallet never negative in history" (`balance_after < 0` on any Wallet Entry).
2. **Circulation masks overspends.** The scenario became one-way: funded senders pay receivers that never send.

Side observation: 92% of requests were rejected even though Wallets held 825–1262 at the end. The capture worker (4 loops) ran behind accepts, so active Holds kept Available near zero during the run. This is worth revisiting at Rung 3.

### Run B: one-way senders → receivers (5 senders × 1000, 5 receivers)
| Metric | Value |
|---|---|
| Requests | 510,330 (16,885 req/s) |
| Accepted / rejected | 49 / 510,281 |
| gRPC p50 / p95 / p99 | 2.54 / 4.98 / 6.20 ms |
| Checker | **FAIL, invariant 3** |
| Senders negative at end | 5 of 5 (−91 to −178) |
| Negative Entries in history | 12 |

Every sender was overspent. The senders drained within the first moments, so nearly all the load after that exercised the rejection path.

## Baseline (P1.10)
Neither run above measures real work. Run A's 1.2k accepted/s was capped by capture lag, and Run B's req/s was almost entirely rejections. The baseline was re-measured with `harness/run.sh baseline`: P2P among 100 amply funded Wallets at a fixed arrival RATE, so almost every request is accepted and the metric is **posted/s end to end** (Accept + Capture). The sustained rate is the highest RATE with no failures and no capture backlog after load.

### As built: capture collapsed
| RATE | Accepted/s | Posted/s end to end | p99 | Backlog after load | Failed (expired) |
|---|---|---|---|---|---|
| 1,000 | 994 | 1,000 | 2.21 ms | 0 | 0 |
| 2,000 | 1,989 | 2,000 | 3.88 ms | 0 | 0 |
| 4,000 | 3,973 | **566** | 13.3 ms | 100k | 2,034 |
| 8,000 | 4,928 | **391** | 196 ms | 135k | 2,426 |

Capture didn't just fall behind, it got *slower* as history grew. `EXPLAIN` showed the claim query (`ORDER BY id LIMIT 1 … SKIP LOCKED`) walking the primary key past 149k captured and expired Holds: 28 ms per claim. Fix: migration 00002 adds a partial index `holds (id) WHERE status = 'active'` (A§4).

### With the capture index
| RATE | Accepted/s | Posted/s end to end | p99 | Backlog after load | Failed |
|---|---|---|---|---|---|
| 2,000 | 1,989 | 1,989 | 3.78 ms | 0 | 0 |
| 4,000 | 3,976 | 2,211 | 11.1 ms | 94k, drained in 24s | 0 |
| 8,000 | 4,728 (3.2k/s dropped by k6) | 1,221 | 232 ms | 124k | 3,403 |

- **Baseline: 2,000 posted/s sustained, p99 3.78 ms.** Config: 1 api, 1 worker × 4 capture loops, 100 Wallets.
- The capture ceiling is about 2.2k posted/s with 4 loops. Accept alone reaches about 4k/s.
- Holds that expire before capture are finalized only by the P2.2 sweeper. Until then they stay `active` and the claim query still scans them.

## Targets (README → Rung Ladder)
| Rung | Target | p99 gate |
|---|---|---|
| 2 | ≥ 2,000 posted/s | ≤ 7.6 ms |
| 3 | ≥ 2,000 posted/s with most traffic on one hot Account | ≤ 7.6 ms |
| 4 | Relative: sharded Postgres vs TigerBeetle as ratios | ≤ 7.6 ms |

## Early Rung 3 evidence
Funding 100 Wallets in setup failed: a TopUp returned `ABORTED` (CAS retries exhausted). Every TopUp debits the single funding System account, and capture loops were posting earlier TopUps against the same row. This is exactly A§8's Rung 3 prediction. The harness now retries `ABORTED` with the same Idempotency-Key, as the A§7 contract says. The ledger is unchanged, because the hot-Account strategy belongs to Rung 3. Related Rung 3 question: Accept bumps the version of System-account sources too, although they have no funds check to protect.

## Lessons
1. **A current-state check can't see a race that later writes repair.** The double-spend happened in the first run but was invisible. History (`balance_after`) makes the check deterministic.
2. **The workload shape decides what's observable.** Circulating money masked overspends. One-way flow made them permanent.
3. **A single-round race test is a coin flip** (about 40% detection). Rounds plus a start barrier made it 10 of 10.
4. **Shared test infrastructure leaks.** A leftover worker container captured the tests' Holds.
5. **Measure the metric that matters.** req/s hid both a capture bottleneck and a query that got slower as history grew. Posted/s end to end exposed both.

## Fix (P1.9): the hypothesis held
The fix is a version CAS on the source Account in Accept, with no balance change (A§5 step 4; Decision Log → *Placing a Hold bumps the Account version*). A loser either blocks on the winner's row lock or sees a changed version. Either way it retries with a fresh read, sees the winner's Hold, and is rejected for insufficient funds.

### Run B again, with the fix (same load)
| Metric | Naive | Fixed |
|---|---|---|
| Requests/s | 16,885 | 16,380 |
| Accepted / rejected | 49 / 510,281 | 49 / 495,061 |
| gRPC p99 | 6.20 ms | 6.21 ms |
| Checker | FAIL (5/5 senders negative, 12 negative Entries) | **CLEAN** |
| Retries exhausted (ABORTED) | — | 0 |

The fix costs no measurable throughput at this contention level. Rung 3's hot-Account load is where CAS retries are expected to hurt.

### Regression test
`TestConcurrentAcceptsCannotOverspend` runs 20 racers from a start barrier, each trying to send 60 out of 100, over 10 rounds. Exactly one may win each round.
- Against the naive Accept: fails 10 of 10 runs.
- Against the fix: passes 10 of 10 runs under `-race`.

A single round caught the naive code only about 40% of the time (12 of 30), which is why it runs 10.

### Test-isolation bug found along the way
The harness left the api and worker containers running, and that worker captured Holds created by the integration tests in the same Postgres. The test's `drain` then saw "nothing to claim" (`SKIP LOCKED` skipped the in-flight capture) before that capture had committed. Fixes:
- `run.sh` stops api and worker at the end.
- `drain` now waits until no active Hold remains.
