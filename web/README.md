# Sutra — web

Next.js front end for the Sutra Mini case study. One page, `/sync`, lists vouchers grouped by status (pending / pushed / failed) with attempt counts, failure reasons, and a Retry button; `/` redirects to it.

The page is server-rendered per request and talks to the Go API (`API_BASE_URL`, default `http://localhost:8080`). The Retry button calls the same-origin `/api/retry/[id]` route, which proxies to the API, so the Go service needs no CORS setup. Press `R` to refresh.

See the repository root [README](../README.md) for the full run instructions.
