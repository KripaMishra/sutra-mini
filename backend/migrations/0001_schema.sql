-- Sutra Mini schema.
--
-- The brief's five tables, kept as given, with deliberate changes:
--   * skus.rate_paise   — the brief's SKU carries no price, so
--                         vouchers.amount_paise would have no source.
--                         Receipts price at the SKU's rate.
--   * vouchers gains next_attempt_at / lease_until / lease_token — the sync
--                         worker's claim-and-retry state (crash-safe and
--                         safe for two worker instances).
--
-- Money is integer paise everywhere; never a float.

CREATE TABLE skus (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code       text   NOT NULL UNIQUE,
    name       text   NOT NULL,
    image_url  text   NOT NULL DEFAULT '',
    rate_paise bigint NOT NULL CHECK (rate_paise > 0)
);

CREATE TABLE parties (
    id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name  text NOT NULL,
    phone text NOT NULL DEFAULT ''
);

CREATE TABLE jobwork_issues (
    id        bigint  GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    party_id  bigint  NOT NULL REFERENCES parties (id),
    sku_id    bigint  NOT NULL REFERENCES skus (id),
    qty       integer NOT NULL CHECK (qty > 0),
    issued_on date    NOT NULL
);

CREATE TABLE jobwork_receipts (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    issue_id        bigint      NOT NULL REFERENCES jobwork_issues (id),
    qty             integer     NOT NULL CHECK (qty > 0),
    received_on     date        NOT NULL,
    idempotency_key text        NOT NULL UNIQUE,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX jobwork_receipts_issue_id_idx ON jobwork_receipts (issue_id);

-- No overshoot, enforced by the database itself: every insert takes a
-- per-issue advisory transaction lock and re-checks the running total inside
-- the transaction, so concurrent inserters serialize and the invariant holds
-- no matter which code path (or client) writes receipts.
CREATE FUNCTION jobwork_receipts_guard_overshoot() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    issued  integer;
    already bigint;
BEGIN
    PERFORM pg_advisory_xact_lock(NEW.issue_id);

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

CREATE TRIGGER jobwork_receipts_guard_overshoot
    BEFORE INSERT ON jobwork_receipts
    FOR EACH ROW
    EXECUTE FUNCTION jobwork_receipts_guard_overshoot();

CREATE TABLE vouchers (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    type            text        NOT NULL,
    source_id       bigint      NOT NULL,
    amount_paise    bigint      NOT NULL CHECK (amount_paise >= 0),
    status          text        NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending', 'pushed', 'failed')),
    attempts        integer     NOT NULL DEFAULT 0,
    last_error      text        NOT NULL DEFAULT '',
    client_ref      text        NOT NULL UNIQUE,
    external_ref    text        NOT NULL DEFAULT '',
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_until     timestamptz,
    lease_token     text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX vouchers_claim_idx ON vouchers (status, next_attempt_at);

CREATE FUNCTION vouchers_touch_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

CREATE TRIGGER vouchers_touch_updated_at
    BEFORE UPDATE ON vouchers
    FOR EACH ROW
    EXECUTE FUNCTION vouchers_touch_updated_at();
