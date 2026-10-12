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
| `POST /ask` (stretch) | Go · langchain-go · GORM · OpenAI-compatible LLM |

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
├── Makefile          one place to run everything (`make help`)
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
make migrate           # schema + seed, then prints the row counts (idempotent)
```

## Run it

```sh
make dev    # api + mock + worker + web together — Ctrl-C stops them all
```

Or run the pieces individually:

```sh
make api      # API on :8080
make mock     # the flaky accounting API on :8090
make worker   # pushes pending vouchers
make web      # the /sync page on :3000 (installs npm dependencies on first run)
```

All of them read the repo's `.env`; under the hood these are `go run ./cmd/...` in
`backend/` and `npm run dev` in `web/`. `make help` lists every target, including `test`
and `build`.

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
| `POST /ask` | Free-text question over read-only tools (`200` answer + tool trace · `503` LLM not configured · `429` queue full · `502` model failure) |
| `GET /healthz` | database ping |

## Ask (the stretch goal)

`POST /ask` answers questions like "how much of SKU X is still with job-worker Y?" with an
LLM that can only read the database. It answers from the same ledger the other endpoints
use — `sum(issued) − sum(received)` per (SKU, job-worker) — and every tool call is logged
*and* returned with the answer:

```sh
curl -s localhost:8080/ask -H 'Content-Type: application/json' \
  -d '{"question":"how much of CHK-01 is still with Meera Embroidery Works?"}'
```

The answer comes back with the tool calls behind it, as structured JSON (not JSON-in-strings):

```json
{
  "question": "how much of CHK-01 is still with Meera Embroidery Works?",
  "answer": "120 pieces were issued and 0 received, so 120 pieces are still with them.",
  "model": "deepseek/deepseek-v4.1-flash",
  "duration_ms": 12057,
  "tool_calls": [
    { "name": "find_skus", "args": { "query": "CHK-01" },
      "result": { "count": 1, "matches": [ "…" ] }, "duration_ms": 2668 },
    { "name": "stock_with_job_worker",
      "args": { "sku": "CHK-01", "job_worker": "Meera Embroidery Works" },
      "result": { "count": 1, "total_still_with": 120, "rows": [ "…" ] }, "duration_ms": 1681 }
  ]
}
```

- **Tools** — five read-only tools (`find_skus`, `find_job_workers`,
  `stock_with_job_worker`, `stock_by_sku`, `stock_by_job_worker`), all GORM queries inside a
  `READ ONLY` transaction with a 5 s statement timeout. There is no write tool and no
  raw-SQL tool; the schema lives in the system prompt.
- **Queue** — three questions run at once (`ASK_CONCURRENCY`, default 3); the rest wait in a
  bounded queue and get `429` + `Retry-After` if it stays full (`ASK_QUEUE_WAIT`, default
  45 s). One process, no extra infrastructure.
- **Model** — any OpenAI-compatible endpoint, via langchain-go: `LLM_API_KEY`, `LLM_MODEL`
  (default `deepseek/deepseek-v4.1-flash`), `LLM_BASE_URL` (no default — where inference
  goes is an environment decision). With any of the three missing the endpoint answers
  `503` naming the variable rather than guessing or stubbing silently.
- **Timeouts** — `/ask` gets its own 60 s budget; every other route stays at 30 s.

## Tests

The guarantees are integration-tested against the real Postgres from `.env`; each test
creates and removes its own rows, and the checks compare against the database instead of
assuming nothing else is moving — so `make test` is safe to run while the demo stack is up:

```sh
make test
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
- `POST /ask` (the optional LLM stretch goal) is built — see
  [Ask](#ask-the-stretch-goal). It is read-only by construction and reports a missing
  `LLM_*` variable instead of stubbing the model.

Built with coding agents — the candid notes, including what they got wrong, live in
[`AGENT_NOTES.md`](AGENT_NOTES.md).
