CREATE TEMP TABLE brand (
    id uuid PRIMARY KEY,
    title varchar NOT NULL,
    image varchar,
    description varchar,
    manufacturer varchar,
    importer varchar,
    created_at timestamptz NOT NULL,
    updated_at timestamptz,
    is_deleted boolean NOT NULL
);

CREATE TEMP TABLE category (
    id uuid PRIMARY KEY,
    title varchar NOT NULL,
    description varchar,
    created_at timestamptz NOT NULL,
    updated_at timestamptz,
    is_deleted boolean NOT NULL
);

CREATE TEMP TABLE banner (
    id uuid PRIMARY KEY,
    title varchar(255) NOT NULL,
    subtitle varchar(1000) NOT NULL,
    image varchar(1024) NOT NULL,
    button_text varchar(100) NOT NULL,
    button_link varchar(2048) NOT NULL,
    sort_order integer NOT NULL CHECK (sort_order BETWEEN 0 AND 10000),
    is_active boolean NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
