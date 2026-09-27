# payment-ledger — project rules

This file overrides the global and workspace CLAUDE.md files wherever they conflict. Anything not covered here follows those files.

## Sources of truth
- Requirements are in `README.md`, domain terms in `CONTEXT.md`, design and the `## Decision Log` in `ARCHITECTURE.md` (A§n), and the task order and checklist in `PLAN.md`.
- For CODE work, read the current `PLAN.md` item and the A§ sections it cites. Flip the checklist marker in the same change that finishes the item.
- Name code after `CONTEXT.md` terms: `Hold`, `Capture`, `Release`, `Receivable`, `Entry`, `Transfer`, `Wallet`, `SystemAccount`. If a new domain concept needs a name, add it to `CONTEXT.md` first.

## Locked stack
Go, connect-go over h2c, pgx v5 + pgxpool, goose, PostgreSQL via Docker Compose, buf, k6. Rationale is in A§2–3. Adding a dependency outside A§3 needs the tradeoff discussion and an A§3 update first.

## Ledger conventions (A§4–5)
- Money is `int64` minor units, always positive. Direction (`debit`/`credit`) carries the sign. Each Transfer debits its source and credits its destination.
- Each write path runs in exactly one transaction, using the store's tx helper.
- Account rows change only through the CAS helper (`version = version + 1 WHERE version = $v`), with bounded retry. When one tx touches several Accounts, update them in ascending `id` order.
- Placing a Hold bumps the source Account's version. A Reversal bumps the debtor Wallet's version.
- Insert Entries after the CAS succeeds, with `created_at = clock_timestamp()` plus `balance_after` and `account_version`.
- Available balance is always derived (posted − active unexpired Holds). A Hold with `expires_at IS NULL` never expires.
- IDs are UUIDv7, generated in Go before insert.
- Errors come from the A§7 table: Connect code + `reason` + human message. New reasons go into A§7 first.
- Failpoints use the names in A§5 exactly. Each is a no-op unless its env var enables it.

## Database
- Schema changes go in a new goose migration under `migrations/`. Applied migrations stay unchanged.
- Tests that touch SQL run against the Compose Postgres. The invariant checker (`cmd/checker --once`) must be clean after every integration or harness run.

## Generated and ask-first files
- `gen/` is buf output and is committed. Regenerate it with `go tool buf generate` after editing `proto/`. Codegen tools are pinned through go.mod's `tool` directive. k6 runs from its Docker image, and `go tool buf curl` is the smoke client.
- Ask before editing: `deploy/docker-compose.yml`, `.gitignore`.

## Rungs
A Rung is done only when it meets all four criteria in README → Success Criteria, including `retros/rung-N.md`. Record Rung-level design choices (hot-Account strategy, sharding) in the Decision Log before implementing them.

## Key files
`cmd/{api,worker,checker,seed}`, `internal/{ledger,store,api,failpoint,checker}`, `migrations/`, `proto/ledger/v1/ledger.proto`, `harness/run.sh`, `retros/`.
