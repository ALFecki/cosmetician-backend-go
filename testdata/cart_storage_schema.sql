CREATE TABLE "order" (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    person_id uuid NOT NULL REFERENCES person (id),
    status text NOT NULL,
    is_deleted boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    total_price real NOT NULL,
    discount_amount real,
    promo_code_id uuid REFERENCES promo_code (id),
    cart_revision bigint NOT NULL DEFAULT 0,
    cart_merge_ids uuid[] NOT NULL DEFAULT '{}',
    checkout_key uuid
);

CREATE TABLE order_product (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id uuid NOT NULL REFERENCES "order" (id) ON DELETE CASCADE,
    variant_id uuid NOT NULL REFERENCES product_variant (id),
    quantity integer NOT NULL
);
