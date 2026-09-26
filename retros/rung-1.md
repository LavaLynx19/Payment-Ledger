# Rung 1 retro: lost update / double-spend

Status: **naive run recorded (P1.8)**. The fix (P1.9) and the final retro (P1.10) are still to come.

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

## Fix hypothesis (P1.9)
Version CAS on the source Account in Accept (A§5 step 4; Decision Log → *Placing a Hold bumps the Account version*).
