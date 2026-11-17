\if :apply_import
LOCK TABLE public.person, public."order", public.order_product IN EXCLUSIVE MODE;
\endif

CREATE TEMP TABLE legacy_open_cart AS
SELECT o.id, o.person_id, o.cart_revision
FROM public."order" AS o
JOIN public.person AS p ON p.id = o.person_id
WHERE o.status = 'NotChecked'
  AND o.payment_status = 'Pending'
  AND o.is_deleted = false
  AND p.is_deleted = false
  AND o.checkout_key IS NULL;

CREATE TEMP TABLE legacy_cart_import AS
SELECT s.*
FROM legacy_cart_snapshot AS s
JOIN legacy_open_cart AS o ON o.id = s.order_id AND o.person_id = s.person_id
WHERE o.cart_revision = 0;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM legacy_cart_snapshot AS s
        LEFT JOIN public."order" AS o ON o.id = s.order_id AND o.person_id = s.person_id
        WHERE o.id IS NULL
    ) THEN
        RAISE EXCEPTION 'snapshot order or owner does not match PostgreSQL';
    END IF;
    IF EXISTS (
        SELECT person_id FROM legacy_open_cart
        GROUP BY person_id
        HAVING COUNT(*) > 1 AND BOOL_OR(cart_revision = 0)
    ) THEN
        RAISE EXCEPTION 'multiple open carts for one owner';
    END IF;
    IF EXISTS (SELECT 1 FROM legacy_cart_import WHERE status <> 'NotChecked') THEN
        RAISE EXCEPTION 'snapshot candidate is not a draft';
    END IF;
    IF EXISTS (
        SELECT 1 FROM legacy_cart_import AS s
        CROSS JOIN LATERAL jsonb_to_recordset(s.items) AS item(variant_id uuid, quantity integer)
        LEFT JOIN public.product_variant AS v ON v.id = item.variant_id
        WHERE v.id IS NULL
    ) OR EXISTS (
        SELECT 1 FROM legacy_cart_import AS s
        LEFT JOIN public.promo_code AS p ON p.id = s.promo_code_id
        WHERE s.promo_code_id IS NOT NULL AND p.id IS NULL
    ) THEN
        RAISE EXCEPTION 'snapshot references a missing variant or promo code';
    END IF;
END $$;

SELECT
    (SELECT COUNT(*) FROM legacy_cart_import),
    (SELECT COUNT(*) FROM legacy_open_cart WHERE cart_revision = 0) - (SELECT COUNT(*) FROM legacy_cart_import),
    (SELECT COUNT(*) FROM legacy_cart_snapshot) - (SELECT COUNT(*) FROM legacy_cart_import);

\if :apply_import
DELETE FROM public.order_product AS p
USING legacy_cart_import AS s
WHERE p.order_id = s.order_id;

INSERT INTO public.order_product (order_id, variant_id, quantity)
SELECT s.order_id, item.variant_id, item.quantity
FROM legacy_cart_import AS s
CROSS JOIN LATERAL jsonb_to_recordset(s.items) AS item(variant_id uuid, quantity integer);

UPDATE public."order" AS o SET
    cart_revision = 1,
    promo_code_id = CASE WHEN s.order_id IS NULL THEN o.promo_code_id ELSE s.promo_code_id END
FROM legacy_open_cart AS c LEFT JOIN legacy_cart_import AS s ON s.order_id = c.id
WHERE o.id = c.id AND o.cart_revision = 0;

COMMIT;
\else
ROLLBACK;
\endif
