-- Let the price-history trigger record *why* a price changed.
--
-- V1 established the trigger and the reason it exists: three separate paths
-- update a price (admin UI, bulk import, promotion expiry) and a history with
-- gaps is worse than no history. Making it a trigger means no path can skip it,
-- including a psql session someone opens during an incident.
--
-- What V1 could not capture was the reason, so `price_history.reason` was
-- always NULL — and "the price changed" without "because we matched a
-- competitor" is not the evidence a consumer authority asks for.
--
-- The actor and the reason arrive the same way, through transaction-local
-- settings the caller sets immediately before the UPDATE:
--
--     SET LOCAL souq.actor  = 'usr_01H...';
--     SET LOCAL souq.reason = 'competitor match';
--     UPDATE variants SET price = ... WHERE sku = ...;
--
-- LOCAL, not SESSION. These run on a pooled connection that is handed to the
-- next request the moment this transaction commits; a SESSION setting would
-- attribute the next admin's price change to this one.
--
-- The `true` second argument to current_setting() means "return NULL if unset"
-- rather than raising. Without it, any UPDATE from a session that had not set
-- the variable — a migration, a manual fix, a test — would fail outright.

CREATE OR REPLACE FUNCTION record_price_change() RETURNS TRIGGER AS $$
BEGIN
    IF (TG_OP = 'UPDATE' AND (OLD.price IS DISTINCT FROM NEW.price
                              OR OLD.list_price IS DISTINCT FROM NEW.list_price)) THEN
        INSERT INTO price_history (sku, old_price, new_price, old_list_price, new_list_price,
                                   currency, changed_by, reason)
        VALUES (NEW.sku, OLD.price, NEW.price, OLD.list_price, NEW.list_price,
                NEW.currency,
                coalesce(nullif(current_setting('souq.actor', true), ''), 'system'),
                nullif(current_setting('souq.reason', true), ''));
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- CREATE OR REPLACE FUNCTION rebinds the existing trigger, so it does not need
-- to be recreated. Dropping and recreating it would leave a window — however
-- brief — in which a price could change with no history row.
