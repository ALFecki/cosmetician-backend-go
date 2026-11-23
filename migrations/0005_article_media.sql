ALTER TABLE article
    ADD COLUMN cover_image jsonb,
    ADD COLUMN body_images jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN featured_position smallint UNIQUE CHECK (featured_position BETWEEN 1 AND 3);
