INSERT INTO "order" (
    id, person_id, status, order_id, payment_status, payment_id, payment_form_url, delivery_cost,
    delivery_method, delivery_date, delivery_address, delivery_post_address, is_courier_delivery,
    payment_method, comment, contact_snapshot, is_deleted, created_at, updated_at, total_price,
    discount_amount, promo_code_id, cart_revision
) VALUES
    ('00000000-0000-4000-8000-000000000901', '00000000-0000-4000-8000-000000000601', 'Checked', '20250801-COFFEE', 'Completed',
     'bank-42', 'https://payments.example.invalid/42', 10, 'standart', '2025-08-02T14:30:00Z', 'Тестовый адрес', NULL, true,
     'online', 'У двери', '{"firstName":"Тест","phone":"+375290000001"}', false, '2025-08-01T14:20:00.123450Z', NULL,
     130.2, 6.1, '00000000-0000-4000-8000-000000000711', 4),
    ('00000000-0000-4000-8000-000000000902', '00000000-0000-4000-8000-000000000601', 'Cancelled', '20250803-ARCHIVE', 'Failed',
     NULL, NULL, 0, 'pickup', NULL, NULL, NULL, false, 'cash', NULL, NULL, true, '2025-08-03T14:20:00Z',
     '2025-08-04T00:00:00Z', 99.5, NULL, NULL, 0),
    ('00000000-0000-4000-8000-000000000903', '00000000-0000-4000-8000-000000000601', 'NotChecked', NULL, 'Pending',
     NULL, NULL, 0, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, false, '2025-08-04T10:20:00Z', NULL,
     0, NULL, NULL, 1),
    ('00000000-0000-4000-8000-000000000904', '00000000-0000-4000-8000-000000000602', 'Completed', '20250804-OTHER', 'Completed',
     NULL, NULL, 0, 'post', NULL, NULL, 'Тестовое отделение', false, 'card', NULL, NULL, false,
     '2025-08-04T14:20:00Z', NULL, 10, 0, NULL, 3);

INSERT INTO order_product (order_id, variant_id, quantity, unit_price, unit_cost_price)
VALUES
    ('00000000-0000-4000-8000-000000000901', '00000000-0000-4000-8000-000000000601', 2, 49.5, 18.25),
    ('00000000-0000-4000-8000-000000000901', '00000000-0000-4000-8000-000000000602', 1, 1, NULL),
    ('00000000-0000-4000-8000-000000000901', '00000000-0000-4000-8000-000000000604', 3, NULL, NULL),
    ('00000000-0000-4000-8000-000000000904', '00000000-0000-4000-8000-000000000607', 1, NULL, NULL);

INSERT INTO product_variant_price_history (variant_id, price, cost_price, valid_from, valid_to)
VALUES
    ('00000000-0000-4000-8000-000000000601', 81, 50, '2025-01-01T00:00:00Z', '2025-09-01T00:00:00Z'),
    ('00000000-0000-4000-8000-000000000602', 15, 10, '2024-01-01T00:00:00Z', '2025-08-01T14:20:00.123450Z'),
    ('00000000-0000-4000-8000-000000000602', 20.5, 12.25, '2025-01-01T00:00:00Z', '2025-09-01T00:00:00Z'),
    ('00000000-0000-4000-8000-000000000602', 21.5, 13.25, '2025-07-01T00:00:00Z', '2025-09-01T00:00:00Z');
