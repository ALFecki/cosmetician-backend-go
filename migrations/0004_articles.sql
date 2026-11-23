CREATE TABLE article (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 160),
    summary text NOT NULL CHECK (char_length(summary) <= 350),
    body text NOT NULL CHECK (char_length(body) <= 30000),
    published_at timestamptz,
    CHECK (published_at IS NULL OR body <> '')
);
