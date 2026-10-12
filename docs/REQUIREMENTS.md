# Requirements

The buildable list, extracted from [`MVP_DIRECTIVES.md`](MVP_DIRECTIVES.md). Plain
checklist — the wording of the brief is the authority, this is the work list.

## Data

1. Create these tables on Postgres (Neon): `skus`, `parties`, `jobwork_issues`,
   `jobwork_receipts`, `vouchers`. Changing the design is allowed, but then the schema
   ships as migrations.
2. Seed 5 SKUs with image URLs, 2 job-workers, 4 open issues.
3. Money is stored as integer paise, never a float.

## Receipts

4. `POST /jobwork/receipts` takes `{issue_id, qty, received_on}`.
5. Total received must never exceed issued quantity for an issue.
6. Rule 5 holds when several receipts arrive at the same moment.
7. The rule is enforced by the database, not by application memory.
8. The request accepts an `Idempotency-Key` header.
9. Replaying the same key returns the same result and creates nothing twice.
10. The voucher row is written in the same transaction as the receipt.

## Read and retry endpoints

11. `GET /issues/{id}` returns issued, received and outstanding.
12. `GET /vouchers?status=` lists vouchers, filtered by status.
13. `POST /vouchers/{id}/retry` requeues a failed voucher.

## Mock accounting server

14. One endpoint: `POST /api/vouchers`.
15. 20% of calls return HTTP 500.
16. 10% of calls hang for 5 seconds, then succeed.
17. A duplicate `client_ref` returns HTTP 409 carrying the original `external_ref`.

## Sync worker

18. Pushes pending vouchers to the accounting API.
19. Retries with backoff.
20. A voucher that keeps failing is parked as `failed` with a reason recorded.
21. A voucher is never posted twice, even if the worker crashes between the HTTP call
    and the database update.
22. Two worker instances can run at the same time without double-posting.
23. The HTTP client has timeouts.
24. The 5-second hang does not block other vouchers.

## Sync page (Next.js)

25. One page at `/sync`.
26. Vouchers grouped by status.
27. Each voucher shows attempt count and failure reason.
28. Each voucher has a Retry button.

## Tests

29. A test proving overshoot is impossible under concurrent receipts.
30. A test proving an idempotency-key replay creates nothing.
31. Both tests fail if the guarantee is broken.

## Delivery

32. README that runs from a clean machine.
33. `AGENT_NOTES.md` with tools used, what they got wrong, and one thing caught.
34. 3-minute screen recording showing a retry succeeding and a duplicate rejected.
35. Public GitHub repo.
36. No secrets committed; keys live in `.env` and a `.env.example` ships.

## Stretch (only if finished early)

- [x] 37. `POST /ask` answers "how much of SKU X is still with job-worker Y?" using an LLM
      with read-only database tools, logging every tool call. Built 2026-10-12; by explicit
      user decision a missing key answers `503` naming the variable instead of stubbing the
      model, and the agent loop is exercised in tests with a fake `llms.Model`.

## How it will be tested

They run the README on a clean machine, then:

- replay a request with the same key → nothing is created twice;
- fire ten concurrent receipts → no overshoot;
- kill the worker during the 5-second hang and restart it → the voucher still lands once;
- run two workers at once → no duplicate posts.
