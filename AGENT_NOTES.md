# AGENT_NOTES

Candid notes on how this was built: which tools were used, where they went wrong, and
what I caught. Appended as the work happens, not reconstructed afterwards.

The brief asks specifically for: tools used, what they got wrong, and one thing I caught.
Those three are kept explicit below.

## Tools used

- Delta (agent harness) with a Go/Postgres/Next.js-capable model for scaffolding,
  migrations and implementation.
- Reading: `pdftotext -layout` to transcribe the brief into
  [`docs/MVP_DIRECTIVES.md`](docs/MVP_DIRECTIVES.md).

## What the tools got wrong

- **Ran terminal commands in the wrong repository.** The agent harness keeps its own
  checkout at `SRS/.delta/worktrees/<id>/SRS`, separate from the primary checkout at
  `SRS/`. A `cp` of the brief and a `mkdir` were issued against the primary checkout and
  landed there instead of in the worktree. Caught by `ls` showing none of the files the
  edit tool had just reported writing. Fixed by deleting the stray `docs/` from the
  primary checkout — which was empty beforehand — and redoing the copy in the worktree.
  No user data was touched.

## Things I caught

- **The provided schema has no price, so `vouchers.amount_paise` has no source.** `skus`
  is `(id, code, name, image_url)` and nothing else carries a rate. The brief permits
  changing the design as long as migrations ship, so the fix is a `rate_paise` column on
  `skus` and `amount_paise = qty * rate_paise`, computed in integer arithmetic. Raised
  with the user before writing code.
- **Idempotency and overshoot need different mechanisms, and neither can live in the
  application.** Overshoot is a write-skew on the sum of receipts for one issue;
  idempotency is a uniqueness rule on the key. Both are database constraints.

## Log

### 2026-10-10 — Repository initialization

- Initialized the Git repository, vendored the brief PDF into `docs/`, and transcribed it
  to `docs/MVP_DIRECTIVES.md` as the working source of truth.
- Added `README.md`, `.env.example` and `.gitignore`.
- No application code yet; `backend/` and `web/` land in the first build commit.
- Committed as `920b111` on branch `main` (renamed from `master` for the eventual public
  GitHub repo).
