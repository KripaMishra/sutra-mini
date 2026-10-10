-- Harden the receipt guards after a review pass.
--
-- 1. The overshoot check relies on READ COMMITTED statement snapshots: the
--    advisory lock serializes writers, and the re-read after the lock sees the
--    latest committed rows. A writer at REPEATABLE READ would keep an older
--    snapshot and could slip past the check, so the trigger now rejects any
--    stronger isolation outright — the invariant fails safe instead of
--    silently breaking. The connection pool pins READ COMMITTED, so
--    application traffic never trips this.
-- 2. Reusing an idempotency key with a different body used to short-circuit
--    silently; it now raises a distinct 'key_reuse_mismatch' so the API can
--    answer 409 instead of echoing the original receipt for a different
--    request.
-- 3. jobwork_receipts becomes append-only (the invariant math assumes rows are
--    never rewritten) and jobwork_issues.qty can no longer drop below what is
--    already received.
-- 4. One voucher per source row becomes a database constraint.

CREATE OR REPLACE FUNCTION jobwork_receipts_guard_overshoot() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    issued         integer;
    already        bigint;
    existing_issue bigint;
    existing_qty   integer;
    existing_on    date;
BEGIN
    IF current_setting('transaction_isolation') NOT IN ('read committed', 'read uncommitted') THEN
        RAISE EXCEPTION 'receipts must be written at READ COMMITTED isolation (got %)',
            current_setting('transaction_isolation')
            USING ERRCODE = 'P0001', DETAIL = 'isolation_mismatch';
    END IF;

    PERFORM pg_advisory_xact_lock(NEW.issue_id);

    SELECT issue_id, qty, received_on
      INTO existing_issue, existing_qty, existing_on
      FROM jobwork_receipts
     WHERE idempotency_key = NEW.idempotency_key;

    IF FOUND THEN
        IF existing_issue <> NEW.issue_id OR existing_qty <> NEW.qty OR existing_on <> NEW.received_on THEN
            RAISE EXCEPTION 'idempotency key % was already used with a different request body', NEW.idempotency_key
                USING ERRCODE = 'P0001', DETAIL = 'key_reuse_mismatch';
        END IF;
        -- Same key, same body: skip the row; the caller's INSERT reports zero
        -- rows and replays the stored receipt.
        RETURN NULL;
    END IF;

    SELECT qty INTO issued FROM jobwork_issues WHERE id = NEW.issue_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'issue % does not exist', NEW.issue_id
            USING ERRCODE = 'P0001', DETAIL = 'issue_not_found';
    END IF;

    SELECT COALESCE(sum(qty), 0) INTO already
      FROM jobwork_receipts
     WHERE issue_id = NEW.issue_id;

    IF already + NEW.qty > issued THEN
        RAISE EXCEPTION 'overshoot: issue % issued %, already received %, new receipt %',
            NEW.issue_id, issued, already, NEW.qty
            USING ERRCODE = 'P0001', DETAIL = 'overshoot';
    END IF;

    RETURN NEW;
END;
$$;

-- The invariant math assumes receipt rows are never rewritten; make that a
-- database rule instead of a convention.
CREATE FUNCTION jobwork_receipts_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'jobwork_receipts is append-only; correct a receipt with a new compensating record'
        USING ERRCODE = 'P0001', DETAIL = 'receipts_append_only';
END;
$$;

CREATE TRIGGER jobwork_receipts_append_only
    BEFORE UPDATE ON jobwork_receipts
    FOR EACH ROW
    EXECUTE FUNCTION jobwork_receipts_append_only();

-- An issue cannot shrink below what has already come back.
CREATE FUNCTION jobwork_issues_guard_received() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    received bigint;
BEGIN
    IF NEW.qty < OLD.qty THEN
        SELECT COALESCE(sum(qty), 0) INTO received
          FROM jobwork_receipts
         WHERE issue_id = NEW.id;
        IF NEW.qty < received THEN
            RAISE EXCEPTION 'issue % cannot be reduced to %: % already received',
                NEW.id, NEW.qty, received
                USING ERRCODE = 'P0001', DETAIL = 'below_received';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER jobwork_issues_guard_received
    BEFORE UPDATE ON jobwork_issues
    FOR EACH ROW
    EXECUTE FUNCTION jobwork_issues_guard_received();

-- A receipt writes exactly one voucher; make double-writing impossible.
ALTER TABLE vouchers
    ADD CONSTRAINT vouchers_one_per_source UNIQUE (type, source_id);
