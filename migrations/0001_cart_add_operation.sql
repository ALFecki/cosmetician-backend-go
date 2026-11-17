CREATE TABLE cart_add_operation (
    cart_id uuid NOT NULL REFERENCES "order" (id) ON DELETE CASCADE,
    operation_id uuid NOT NULL,
    variant_id uuid NOT NULL,
    PRIMARY KEY (cart_id, operation_id)
);
