ALTER TABLE product_variant ADD COLUMN price_cents bigint, ADD COLUMN cost_price_cents bigint;

CREATE FUNCTION catalog_byn_to_cents(amount real) RETURNS bigint
LANGUAGE sql IMMUTABLE STRICT SET extra_float_digits = 1 AS $$
    SELECT CASE WHEN amount BETWEEN 0 AND 1000000
                THEN round(amount::text::numeric * 100)::bigint
                ELSE -1 END
$$;

CREATE FUNCTION catalog_legacy_price_matches(amount real, cents bigint) RETURNS boolean
LANGUAGE sql IMMUTABLE STRICT SET extra_float_digits = 1 AS $$
    SELECT CASE WHEN amount BETWEEN 0 AND 1000000
                THEN amount::text::numeric = cents::numeric / 100
                     AND floor(amount::double precision * 100 + 0.5) = cents
                ELSE false END
$$;

-- Keep the exact REAL values that the old order reader used as its last fallback.
UPDATE order_product op
SET unit_price = v.price, unit_cost_price = v.cost_price
FROM "order" o, product_variant v
WHERE o.id = op.order_id AND v.id = op.variant_id
  AND (o.status <> 'NotChecked' OR o.payment_status = 'Completed')
  AND (op.unit_price IS NULL OR op.unit_cost_price IS NULL)
  AND NOT EXISTS (
      SELECT 1 FROM product_variant_price_history h
      WHERE h.variant_id = v.id AND h.valid_from <= o.created_at
        AND (h.valid_to IS NULL OR h.valid_to > o.created_at)
  );

UPDATE product_variant
SET price_cents = catalog_byn_to_cents(price), cost_price_cents = catalog_byn_to_cents(cost_price);

ALTER TABLE product_variant
    ALTER COLUMN price_cents SET NOT NULL,
    ALTER COLUMN cost_price_cents SET NOT NULL,
    ADD CONSTRAINT catalog_prices_in_range CHECK (
        price_cents BETWEEN 0 AND 100000000 AND cost_price_cents BETWEEN 0 AND 100000000
    ),
    ADD CONSTRAINT catalog_prices_match_legacy CHECK (
        catalog_legacy_price_matches(price, price_cents) AND catalog_legacy_price_matches(cost_price, cost_price_cents)
    );

CREATE FUNCTION sync_catalog_variant_prices() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        NEW.price_cents = COALESCE(NEW.price_cents, catalog_byn_to_cents(NEW.price));
        NEW.cost_price_cents = COALESCE(NEW.cost_price_cents, catalog_byn_to_cents(NEW.cost_price));
        NEW.price = NEW.price_cents::numeric / 100;
        NEW.cost_price = NEW.cost_price_cents::numeric / 100;
        RETURN NEW;
    END IF;

    IF NEW.price_cents IS NOT DISTINCT FROM OLD.price_cents AND NEW.price IS DISTINCT FROM OLD.price THEN
        NEW.price_cents = catalog_byn_to_cents(NEW.price);
    END IF;
    IF NEW.cost_price_cents IS NOT DISTINCT FROM OLD.cost_price_cents AND NEW.cost_price IS DISTINCT FROM OLD.cost_price THEN
        NEW.cost_price_cents = catalog_byn_to_cents(NEW.cost_price);
    END IF;

    IF NEW.price_cents IS DISTINCT FROM OLD.price_cents THEN
        NEW.price = NEW.price_cents::numeric / 100;
    ELSE
        NEW.price = OLD.price;
    END IF;
    IF NEW.cost_price_cents IS DISTINCT FROM OLD.cost_price_cents THEN
        NEW.cost_price = NEW.cost_price_cents::numeric / 100;
    ELSE
        NEW.cost_price = OLD.cost_price;
    END IF;
    IF NEW.price_cents IS DISTINCT FROM OLD.price_cents OR NEW.cost_price_cents IS DISTINCT FROM OLD.cost_price_cents THEN
        NEW.updated_at = clock_timestamp();
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER sync_catalog_variant_prices
BEFORE INSERT OR UPDATE OF price, cost_price, price_cents, cost_price_cents ON product_variant
FOR EACH ROW EXECUTE FUNCTION sync_catalog_variant_prices();

CREATE OR REPLACE FUNCTION create_price_history() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    effective_at timestamptz := clock_timestamp();
BEGIN
    IF TG_OP = 'UPDATE' THEN
        effective_at = NEW.updated_at;
    END IF;
    UPDATE product_variant_price_history SET valid_to = effective_at, updated_at = effective_at
    WHERE variant_id = NEW.id AND valid_to IS NULL;
    INSERT INTO product_variant_price_history (variant_id, price, cost_price, valid_from)
    VALUES (NEW.id, NEW.price, NEW.cost_price, effective_at);
    RETURN NEW;
END;
$$;

DROP TRIGGER trigger_price_history ON product_variant;

CREATE TRIGGER trigger_price_history_on_insert
AFTER INSERT ON product_variant FOR EACH ROW EXECUTE FUNCTION create_price_history();

CREATE TRIGGER trigger_price_history
AFTER UPDATE ON product_variant FOR EACH ROW
WHEN (OLD.price_cents IS DISTINCT FROM NEW.price_cents OR OLD.cost_price_cents IS DISTINCT FROM NEW.cost_price_cents)
EXECUTE FUNCTION create_price_history();

CREATE TABLE catalog_price_operation (
    operation_id uuid PRIMARY KEY,
    request_digest bytea NOT NULL,
    updated_count integer NOT NULL CHECK (updated_count >= 0)
);
