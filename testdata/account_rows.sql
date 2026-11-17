INSERT INTO person (
    id, first_name, second_name, surname, email, phone, is_email_verified, password,
    image, is_moderator, is_admin, is_deleted, created_at, updated_at,
    verified_phone, phone_verified_at, contact_email, checkout_details
) VALUES
    ('00000000-0000-4000-8000-000000000601', 'Тест', 'Покупатель', NULL, 'test@example.invalid',
     '+375290000001', false, '$2b$04$sDPFlhzCbEPYv8v7.bKlL.r/7iGC2mBWO6oogwXllgx/tXJnyLm3C',
     NULL, false, false, false, '2025-08-09T10:11:12.123450Z', NULL, NULL, NULL, NULL, NULL),
    ('00000000-0000-4000-8000-000000000602', 'Тест', 'Администратор', 'Тестовый', 'admin@example.invalid',
     '+375290000002', true, '$2y$04$sDPFlhzCbEPYv8v7.bKlL.r/7iGC2mBWO6oogwXllgx/tXJnyLm3C',
     'photo.png', true, true, false, '2025-08-09T10:11:12Z', '2025-08-10T10:11:12Z', NULL, NULL, NULL, NULL),
    ('00000000-0000-4000-8000-000000000603', 'Тест', 'Телефон', NULL, 'phone@example.invalid',
     '+375290000003', false, NULL, NULL, false, false, false, '2025-08-09T10:11:12Z', NULL,
     '+375290000003', '2025-08-10T10:11:12Z', 'contact@example.invalid', '{"city":"Минск"}'),
    ('00000000-0000-4000-8000-000000000604', 'Тест', 'Архив', NULL, 'archived@example.invalid',
     '+375290000004', false, '$2b$04$sDPFlhzCbEPYv8v7.bKlL.r/7iGC2mBWO6oogwXllgx/tXJnyLm3C',
     NULL, false, false, true, '2025-08-09T10:11:12Z', NULL, NULL, NULL, NULL, NULL),
    ('00000000-0000-4000-8000-000000000605', 'Тест', 'Пароль', NULL, 'long@example.invalid',
     '+375290000005', false, '$2a$05$......................YgIDy4hFBdVlc/6LHnD9mX488r9cLd2',
     NULL, false, false, false, '2025-08-09T10:11:12Z', NULL, NULL, NULL, NULL, NULL);
