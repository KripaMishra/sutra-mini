# AGENT_NOTES

Candid notes on how this was built: which tools were used, where they went wrong, and
what I caught. Appended as the work happens, not reconstructed afterwards.

The brief asks specifically for: tools used, what they got wrong, and one thing I caught.
Those three are kept explicit below.

## Tools used

- Reading: `pdftotext -layout` to transcribe the brief into
  [`docs/MVP_DIRECTIVES.md`](docs/MVP_DIRECTIVES.md).
- CommandCode (this agent) for the implementation pass after the brief: schema and
  migrations, the chi API, the sync worker, the mock accounting server, the Next.js page
  and the tests. A sub-agent code review gated every branch (several findings below);
  the `neon` CLI handled the project link, deploy, and the agent skills/MCP setup.

## What the tools got wrong

- **The plain `neon skills -y` quickstart failed** with "no coding agents detected in
  this project" until the target agents were named explicitly (`--agent ...`) — the guide
  assumes the command runs somewhere Neon recognises.
- **The first overshoot trigger mis-fired on replays.** It runs before the unique index
  is checked, so replaying a key on an almost- or fully-received issue counted the stored
  row again and raised a false overshoot instead of replaying. Caught while writing the
  API; fixed in migration 0002 and hardened further in 0003.
- **A review pass found a replay window**: a concurrent reuse of the same key with a
  *different* body could come back as `200 replayed` with the original receipt. The
  reload path now verifies issue/qty/date and answers `409` deterministically.
- **The web build failed under Next 16 Cache Components** until the data fetch moved
  inside a `<Suspense>` boundary — uncached data may not block a prerender.
- **langchain-go's function-calling agent cannot describe a tool's arguments.** Before
  writing `POST /ask`, reading `llms/openai`'s sibling agent showed
  `OpenAIFunctionsAgent` advertises every tool with one generic `__arg1` string parameter —
  no per-tool JSON schema. `stock_with_job_worker(sku, job_worker)` needs two typed
  arguments, so the loop is hand-rolled over langchain-go's `llms` primitives (client,
  `Tool`/`FunctionDefinition`, tool-call message parts) instead of `agents.Executor`. Caught
  by reading the vendored package first; the alternative would have been a model guessing at
  a string-packed argument format.

## Things I caught

- **The provided schema has no price, so `vouchers.amount_paise` has no source.** `skus`
  is `(id, code, name, image_url)` and nothing else carries a rate. The brief permits
  changing the design as long as migrations ship, so the fix is a `rate_paise` column on
  `skus` and `amount_paise = qty * rate_paise`, computed in integer arithmetic. Raised
  with the user before writing code.
- **Idempotency and overshoot need different mechanisms, and neither can live in the
  application.** Overshoot is a write-skew on the sum of receipts for one issue;
  idempotency is a uniqueness rule on the key. Both are database constraints.
- **The overshoot guard silently depended on READ COMMITTED.** A REPEATABLE READ writer
  could hold a stale snapshot past the check — a review harness reproduced two 60-piece
  receipts landing as 120 on a 100-piece issue. The trigger now rejects stronger
  isolation outright, the pool pins READ COMMITTED, receipts are append-only, and an
  issue's quantity cannot drop below what has already come back.
- **"Read-only" needed to be a transaction property, not a promise.** The /ask tools have no
  write path, but that is a statement about today's code. Each tool call instead runs inside
  a `READ ONLY` transaction, and a test inserts a row inside that transaction and asserts
  Postgres refuses it (`SQLSTATE 25006`) — so a future edit that grows a write fails loudly
  instead of quietly.

## Log

### 2026-10-10 — Repository initialization

- Initialized the Git repository, vendored the brief PDF into `docs/`, and transcribed it
  to `docs/MVP_DIRECTIVES.md` as the working source of truth.
- Added `README.md`, `.env.example` and `.gitignore`.
- No application code yet; `backend/` and `web/` land in the first build commit.
- Committed as `920b111` on branch `main` (renamed from `master` for the eventual public
  GitHub repo).

### 2026-10-10 — Build (after the brief)

- Linked the Neon project (`empty-sunset-92775895`, branch `production`), deployed
  `neon.ts`, and installed the Neon agent skills + MCP server for the repo's coding
  agents (with backups of every config touched).
- History, each branch merged with `--no-ff` after a sub-agent review pass:
  `chore/neon-setup` → `feat/db` (schema, guards, seed, migrate runner) → `feat/api`
  (chi endpoints) → `feat/tests` (four guarantee suites) → `feat/worker` (mock accounting
  + sync worker) → `feat/web` (`/sync`) → `docs/final`.
- Verified live on Neon: migrations + seed (5/2/4), create/replay/overshoot smokes, a
  worker smoke (pending → pushed with exactly one record accounting-side), all four
  test suites green, `next build` green.

### 2026-10-12 — `POST /ask` (the stretch item)

- Planned first (vault plan `2026-10-12-ask-endpoint`, seven decisions grilled with the user)
  before writing code: env contract with no default inference URL, an in-process 3-worker
  queue with `429` backpressure, five GORM read tools, `/ask`-scoped 60 s budget, and an
  explicit `503` when a key is missing instead of the brief's "stub the model" — that last
  one is a user decision, recorded in [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md).
- Implemented `backend/internal/ask`: GORM finders in a `READ ONLY` transaction, five typed
  tools (no raw SQL, no write path), a hand-rolled langchain-go tool loop with a per-call
  trace and structured log line, and a bounded queue whose workers survive a panicking job.
- The 503 path was the first live check; the full pipeline was then smoke-tested against a
  local fake OpenAI-compatible server on `:8091` — real HTTP, all five tool schemas on the
  wire, a real Neon read (`issued 80 − received 5 = 75`), and the tool call logged with the
  request id. Once a key was configured, the same question was answered by the real model
  (`deepseek/deepseek-v4.1-flash` through an OpenAI-compatible endpoint) with the tool trace
  and log lines intact. All smoke processes stopped afterwards.
- 36 tests now cover `/ask` (`internal/ask` 24, `internal/httpapi` 12): aggregate math,
  `LIKE` wildcard escaping, same-named workers staying separate, truncation reporting,
  read-only refusal, tool-error recovery, per-turn tool-call cap, step budget, queue cap +
  `429`, panic isolation, shutdown cancellation, and the `400/413/429/502/503/504` mapping.
  `go test ./...` green against the Postgres in `.env`; shipped as `40833c3`/`1c32091` on
  `feat/ask`, merged to `main` (`396f98a`).

### 2026-10-12 — Review pass on `/ask` (before the author's own read)

- Ran four parallel reviewer agents over `c304146..d546d22` (concurrency/lifecycle, SQL and
  read-only semantics, HTTP/config/hygiene, tests-and-claims). They reproduced failures
  against the live database rather than only reading code; a temporary probe file they left
  was deleted before commit.
- Two findings changed behaviour rather than wording:
  - **Two job-workers with the same name were summed into one row.** `parties.name` is not
    unique — the prompt says so — yet the aggregate grouped by name. Rows are now grouped by
    `p.id`/`s.id`, carry `job_worker_id`/`sku_id`, and a note says when the rows belong to
    different people who share a name.
  - **A capped list looked complete.** `total_still_with` was the sum of the first 50 rows and
    `count` stopped at the limit with no hint. Both finders and the aggregate now carry
    `COUNT(*) OVER ()` totals and a `truncated` flag; the aggregate's total covers every
    matched pair even when the rows list does not.
- Also fixed: shutdown now refuses queued questions and cancels in-flight ones instead of
  leaving a job waiting for a worker that has exited; an answer that lands exactly at the
  deadline is returned rather than discarded; one model turn can no longer fan out to
  unbounded tool calls; a nameless tool call no longer produces a transcript providers reject;
  a bad `ASK_*` value disables `/ask` instead of failing the whole API at boot; oversize bodies
  answer `413` and oversize questions `400`; phone numbers are no longer returned or logged;
  the read pool pings at boot; and `TestNewValidatesQueueSettings` — which passed even with the
  validation deleted — now asserts the error message.
