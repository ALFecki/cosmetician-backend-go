CREATE TYPE payment_status AS ENUM ('Pending', 'Processing', 'Completed', 'Failed');

ALTER TABLE "order"
    ADD COLUMN payment_started_at timestamptz,
    ALTER COLUMN payment_status DROP DEFAULT,
    ALTER COLUMN payment_status TYPE payment_status USING payment_status::payment_status,
    ALTER COLUMN payment_status SET DEFAULT 'Pending';

UPDATE "order" SET payment_method = 'online'
WHERE id IN ('00000000-0000-4000-8000-000000000921', '00000000-0000-4000-8000-000000000926');
