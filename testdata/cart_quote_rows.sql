INSERT INTO product_variant (id, product_id, volume, cost_price, price, is_default, is_deleted, created_at, updated_at)
VALUES
    ('00000000-0000-4000-8000-000000000607', '00000000-0000-4000-8000-000000000504', 15, 8, 10, true, false, '2025-01-02T03:04:05Z', NULL),
    ('00000000-0000-4000-8000-000000000608', '00000000-0000-4000-8000-000000000503', 55, 36, 400, true, false, '2025-01-02T03:04:05Z', NULL);

INSERT INTO promo_code (id, code, discount_type, discount_value, min_order_amount, max_discount_amount, usage_limit, used_count, valid_from, valid_until, is_active, applicable_to)
VALUES
    ('00000000-0000-4000-8000-000000000715', 'EXHAUSTED', 'fixed', 25, NULL, NULL, 1, 1, '2020-01-01T00:00:00Z', '2099-01-01T00:00:00Z', true, NULL),
    ('00000000-0000-4000-8000-000000000716', 'FIXED-OVER', 'fixed', 1000, NULL, NULL, NULL, 0, '2020-01-01T00:00:00Z', '2099-01-01T00:00:00Z', true, NULL),
    ('00000000-0000-4000-8000-000000000717', 'PERCENT', 'percentage', 12.7, 39.99, 20, NULL, 0, '2020-01-01T00:00:00Z', '2099-01-01T00:00:00Z', true, NULL);

INSERT INTO promotion (id, title, discount_type, discount_value, target_type, priority, valid_from, valid_until, is_active, created_at, updated_at)
VALUES
    ('00000000-0000-4000-8000-000000000707', 'Бренд', 'fixed', 17, 'brand', 4, '2020-01-01T00:00:00Z', '2099-01-01T00:00:00Z', true, '2025-01-02T03:04:05Z', '2025-01-02T03:04:05Z'),
    ('00000000-0000-4000-8000-000000000708', 'Категория', 'fixed', 19, 'category', 3, '2020-01-01T00:00:00Z', '2099-01-01T00:00:00Z', true, '2025-01-02T03:04:05Z', '2025-01-02T03:04:05Z');
INSERT INTO product_promotion (product_id, promotion_id)
VALUES
    ('00000000-0000-4000-8000-000000000501', '00000000-0000-4000-8000-000000000707'),
    ('00000000-0000-4000-8000-000000000501', '00000000-0000-4000-8000-000000000708');
