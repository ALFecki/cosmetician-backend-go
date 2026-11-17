CREATE TEMP TABLE product_variant_price_history (
    variant_id uuid NOT NULL REFERENCES product_variant (id),
    price real NOT NULL,
    cost_price real NOT NULL,
    valid_from timestamptz NOT NULL,
    valid_to timestamptz
);
