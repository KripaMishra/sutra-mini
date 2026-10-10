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

- a receipt can never overshoot what was issued, even under concurrent requests;
- receipt and voucher are written in one transaction;
- replaying a request with the same `Idempotency-Key` changes nothing;
- a voucher reaches accounting exactly once, surviving crashes, retries and two workers
  running at the same time.

## Stack

| Piece | Technology |
| --- | --- |
| API + sync worker | Go |
| Database | PostgreSQL (Neon, free tier) |
| Sync page | Next.js |
| Mock accounting server | Go |

## Repository layout

```
SRS/
├── docs/
│   ├── MVP_DIRECTIVES.md                  the brief, verbatim
│   ├── REQUIREMENTS.md                    the brief as a buildable checklist
│   └── case_study_brief_radha_studios.pdf the brief as received
├── backend/     Go module: HTTP API, sync worker, mock accounting server, migrations  (planned)
├── web/         Next.js app: the /sync page                                       (planned)
├── AGENT_NOTES.md
└── .env.example
```

## Status

**Scaffolding only.** The repository is initialized and the brief is recorded. No service
runs yet — the sections below describe the intended setup and are filled in as each piece
lands. Nothing under `backend/` or `web/` exists until the first build commit.

## Prerequisites

- Go 1.24 or newer
- Node.js 20 or newer
- A Neon Postgres database (free tier is enough), or Docker for a local Postgres

## Setup

```sh
cp .env.example .env    # then fill in DATABASE_URL
```

Run instructions are added as the API, worker and web app land.
