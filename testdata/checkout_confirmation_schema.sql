ALTER TABLE "order"
    ADD COLUMN checkout_request_hash text,
    ADD UNIQUE (checkout_key);

CREATE TABLE promo_code_usage (
    promo_code_id uuid NOT NULL REFERENCES promo_code (id),
    order_id uuid NOT NULL REFERENCES "order" (id),
    person_id uuid NOT NULL REFERENCES person (id),
    discount_amount real NOT NULL
);

UPDATE promo_code SET used_count = 0 WHERE code = 'FIXTURE-ONLY';
