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

_(appended as it happens)_

## Things I caught

_(appended as it happens)_

## Log

### 2026-10-10 — Repository initialization

- Initialized the Git repository, vendored the brief PDF into `docs/`, and transcribed it
  to `docs/MVP_DIRECTIVES.md` as the working source of truth.
- Added `README.md`, `.env.example` and `.gitignore`.
- No application code yet; `backend/` and `web/` land in the first build commit.
