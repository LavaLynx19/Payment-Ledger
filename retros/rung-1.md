# Rung 1 retro: lost update / double-spend

Status: **fix verified (P1.9)**. Targets and the final retro (P1.10) are still to come.

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

## Baseline (for Rung 2-4 targets)
- Run A is the better throughput baseline, because it exercises Accept + Capture: **~15.3k req/s, ~1.2k accepted/s, p99 6.66 ms**.
- Run B's rate is dominated by cheap rejections, so it isn't comparable.

Targets are set in P1.10.

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
