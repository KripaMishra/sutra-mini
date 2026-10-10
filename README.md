# SRS — Sutra Mini

A reliable layer between job-work receipts and an accounting API that is slow and fails
without warning. The smallest honest version of it.

This repository is the case-study submission for Shree Radha Studio. The requirements are
fixed by the brief:

- [`docs/MVP_DIRECTIVES.md`](docs/MVP_DIRECTIVES.md) — the brief as received, verbatim.
- [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md) — the same thing as a buildable checklist.

Read the directives first.

## What it does

Fabric goes out to embroidery and stitching job-workers and comes back in parts over days.
Each receipt has an accounting consequence. Sutra makes sure that:

- a receipt can never overshoot what was issued, even under concurrent requests — enforced
  by a Postgres trigger, not by application code;
- receipt and voucher are written in one transaction;
- replaying a request with the same `Idempotency-Key` changes nothing;
- a voucher reaches accounting exactly once, surviving crashes, retries and two workers
  running at the same time.

## Stack

| Piece | Technology |
| --- | --- |
| API + sync worker + mock accounting | Go 1.25, chi, pgx |
| Database | PostgreSQL (Neon free tier; any Postgres works) |
| Migrations | Embedded SQL, applied by `backend/cmd/migrate` |
| Sync page | Next.js 16 (App Router, Tailwind) |

## How it works

`jobwork_issues` (fabric out) → `POST /jobwork/receipts` (comes back; one transaction
writes the receipt and its voucher) → the sync worker pushes pending vouchers to the
accounting API with retries and backoff → `/sync` shows the state.

Where each guarantee lives:

- **No overshoot** — a `BEFORE INSERT` trigger on `jobwork_receipts` takes a per-issue
  advisory lock and re-checks the running total; it fails safe at stronger isolation
  levels, and receipts are append-only.
- **Idempotency** — `UNIQUE (idempotency_key)`; a replay returns the stored receipt and
  voucher (`"replayed": true`) and creates nothing. Reusing a key with a different body
  is a `409`.
- **Exactly-once push** — the worker claims vouchers with `FOR UPDATE SKIP LOCKED` plus an
  expiring lease; the accounting side deduplicates on `client_ref`, and its `409`
  (duplicate) response is reconciled to the original reference — so a crash between the
  HTTP call and the database update still ends in `pushed`, never a double post. Two
  workers can run side by side.
- **Money** — integer paise everywhere; `skus.rate_paise` is the rate the brief's schema
  was missing (vouchers price at `qty × rate_paise`).

## Repository layout

```
SRS/
├── backend/          Go module: API, sync worker, mock accounting, migrations, tests
├── web/              Next.js app: the /sync page
├── docs/             the brief, verbatim + as a checklist
├── neon.ts           Neon config-as-code (the project link lives in .neon, gitignored)
├── AGENT_NOTES.md    how this was built, with the mistakes
└── .env.example      copy to .env and fill in DATABASE_URL
```

## Prerequisites

- Go 1.25 or newer
- Node.js 20 or newer
- A Postgres database — Neon's free tier is enough; any Postgres works (Neon needs TLS,
  so keep `sslmode=require` in the connection string)

## Setup

```sh
cp .env.example .env    # fill in DATABASE_URL (e.g. the pooled Neon connection string)
```

Apply the schema and seed the brief's data (5 SKUs, 2 job-workers, 4 open issues):

```sh
cd backend
go run ./cmd/migrate -seed -check
```

## Run it

Four processes, all reading the repo's `.env`:

```sh
cd backend && go run ./cmd/api             # API on :8080
cd backend && go run ./cmd/mockaccounting  # the flaky accounting API on :8090
cd backend && go run ./cmd/worker          # pushes pending vouchers
```

```sh
cd web && npm install && npm run dev       # the /sync page on :3000
```

Open http://localhost:3000/sync — vouchers grouped by status with attempt counts, failure
reasons, and a Retry button. Press `R` to refresh.

## Try the guarantees

**Replay creates nothing twice** — run this twice; the second answer is `200` with
`"replayed": true` and nothing changed:

```sh
curl -s -X POST localhost:8080/jobwork/receipts \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: demo-1' \
  -d '{"issue_id":1,"qty":5,"received_on":"2026-10-10"}'
```

**Ten concurrent receipts, no overshoot** — check how much is outstanding, then fire ten
requests of exactly that size; one is created, nine are `409`:

```sh
curl -s localhost:8080/issues/1    # issued / received / outstanding
OUTSTANDING=...                    # from the line above
seq 1 10 | xargs -P 10 -I{} curl -s -o /dev/null -w '%{http_code}\n' \
  -X POST localhost:8080/jobwork/receipts \
  -H 'Content-Type: application/json' -H "Idempotency-Key: race-{}" \
  -d "{\"issue_id\":1,\"qty\":$OUTSTANDING,\"received_on\":\"2026-10-10\"}"
```

**Crash-safe, two-worker** — start two `worker` processes and `kill -9` one mid-push. It
leaves only an expiring lease (default 15 s, `WORKER_LEASE`); the other (or a restarted)
worker re-claims the voucher and pushes it once — if the killed worker had already reached
accounting, the duplicate `409` reconciles to the same reference instead of double-posting.

## API

| Endpoint | Notes |
| --- | --- |
| `POST /jobwork/receipts` | Requires an `Idempotency-Key` header. `201` created · `200` replay · `409` overshoot or key reuse · `404` unknown issue · `400` bad input |
| `GET /issues/{id}` | `issued`, `received`, `outstanding` |
| `GET /vouchers?status=` | `pending` / `pushed` / `failed` (omit for all) |
| `POST /vouchers/{id}/retry` | Requeues a `failed` voucher (`200`), `409` if it isn't failed |
| `GET /healthz` | database ping |

## Tests

The guarantees are integration-tested against the real Postgres from `.env`; each test
creates and removes its own rows:

```sh
cd backend && go test ./...
```

- `TestNoOvershootUnderConcurrentReceipts` — ten simultaneous receipts, one winner, the
  sum never exceeds issued.
- `TestIdempotentReplayCreatesNothing` — sequential and concurrent replays, plus
  key-reuse rejection.
- `TestReceiptsFillToTheBoundaryThenStop` — fills exactly, rejects the next unit, and
  fails safe at stronger isolation levels.
- `TestIssueProgressAndVoucherRetry` — the read endpoints and the failed → pending retry
  contract.

Each fails if its guarantee is broken — drop the trigger, the unique key, or the replay
check and the assertions go red.

## Notes and decisions

- The brief's schema had no price for a receipt, so `skus.rate_paise` was added and every
  voucher prices at `qty × rate_paise`; the change ships as a migration.
- Receipts are append-only and the overshoot guard pins READ COMMITTED — both are
  deliberate, and both are enforced by the database.
- Dates are dates (`YYYY-MM-DD`); money is integer paise.
- `POST /ask` (the optional LLM stretch goal) is intentionally not built.

Built with coding agents — the candid notes, including what they got wrong, live in
[`AGENT_NOTES.md`](AGENT_NOTES.md).
