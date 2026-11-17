CREATE TEMP TABLE "order" (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    person_id uuid NOT NULL REFERENCES person (id),
    status text NOT NULL,
    order_id text,
    payment_status text NOT NULL DEFAULT 'Pending',
    payment_id text,
    payment_form_url text,
    delivery_cost integer NOT NULL DEFAULT 0,
    delivery_method text,
    delivery_date timestamp,
    delivery_address text,
    delivery_post_address text,
    is_courier_delivery boolean,
    payment_method text,
    comment text,
    contact_snapshot jsonb,
    is_deleted boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz DEFAULT now(),
    total_price real NOT NULL,
    discount_amount real,
    promo_code_id uuid REFERENCES promo_code (id),
    cart_revision bigint NOT NULL DEFAULT 0,
    cart_merge_ids uuid[] NOT NULL DEFAULT '{}',
    checkout_key uuid
);

CREATE TEMP TABLE order_product (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id uuid NOT NULL REFERENCES "order" (id) ON DELETE CASCADE,
    variant_id uuid NOT NULL REFERENCES product_variant (id),
    quantity integer NOT NULL,
    unit_price real,
    unit_cost_price real
);
