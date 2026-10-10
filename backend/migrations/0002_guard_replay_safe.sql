-- Make the overshoot guard replay-safe.
--
-- The trigger runs before the unique index is checked, so a replay of an
-- existing idempotency_key used to be counted as a new receipt: on an issue
-- that was already fully received, the replay raised a false overshoot
-- instead of falling through to ON CONFLICT DO NOTHING and replaying the
-- stored row. Two changes fix that without weakening the guard:
--
--   1. An existing key short-circuits the insert (RETURN NULL), so committed
--      duplicates never reach the math at all.
--   2. The running total excludes rows that share the incoming key, which
--      covers an in-flight duplicate still waiting on the unique index.
--
-- Genuine overshoots (a new key that would exceed the issued quantity) still
-- raise exactly as before.

CREATE OR REPLACE FUNCTION jobwork_receipts_guard_overshoot() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    issued  integer;
    already bigint;
BEGIN
    PERFORM pg_advisory_xact_lock(NEW.issue_id);

    IF EXISTS (SELECT 1 FROM jobwork_receipts WHERE idempotency_key = NEW.idempotency_key) THEN
        RETURN NULL;
    END IF;

    SELECT qty INTO issued FROM jobwork_issues WHERE id = NEW.issue_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'issue % does not exist', NEW.issue_id
            USING ERRCODE = 'P0001', DETAIL = 'issue_not_found';
    END IF;

    SELECT COALESCE(sum(qty), 0) INTO already
      FROM jobwork_receipts
     WHERE issue_id = NEW.issue_id
       AND idempotency_key <> NEW.idempotency_key;

    IF already + NEW.qty > issued THEN
        RAISE EXCEPTION 'overshoot: issue % issued %, already received %, new receipt %',
            NEW.issue_id, issued, already, NEW.qty
            USING ERRCODE = 'P0001', DETAIL = 'overshoot';
    END IF;

    RETURN NEW;
END;
$$;
