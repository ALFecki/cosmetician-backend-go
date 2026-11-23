CREATE TEMP TABLE product_variant_price_history (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    variant_id uuid NOT NULL REFERENCES product_variant (id) ON DELETE CASCADE,
    price real NOT NULL,
    cost_price real NOT NULL,
    valid_from timestamptz NOT NULL DEFAULT now(),
    valid_to timestamptz,
    created_at timestamptz DEFAULT now(),
    updated_at timestamptz DEFAULT now()
);

CREATE FUNCTION create_price_history() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE product_variant_price_history SET valid_to = now(), updated_at = now()
      WHERE variant_id = NEW.id AND valid_to IS NULL;
    INSERT INTO product_variant_price_history (variant_id, price, cost_price)
      VALUES (NEW.id, NEW.price, NEW.cost_price);
    RETURN NEW;
END;
$$;

CREATE TRIGGER trigger_price_history
AFTER INSERT OR UPDATE OF price, cost_price ON product_variant
FOR EACH ROW EXECUTE FUNCTION create_price_history();
