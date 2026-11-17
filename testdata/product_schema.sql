CREATE TEMP TABLE product (
    id uuid PRIMARY KEY,
    title varchar NOT NULL,
    full_title varchar NOT NULL,
    image varchar,
    description varchar,
    composition varchar,
    method_of_use varchar,
    brand_id uuid NOT NULL,
    product_type_id uuid NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz,
    is_price_hidden boolean,
    is_deleted boolean NOT NULL
);
CREATE TEMP TABLE product_type (
    id uuid PRIMARY KEY, title varchar NOT NULL, description varchar,
    created_at timestamptz NOT NULL, updated_at timestamptz, is_deleted boolean NOT NULL DEFAULT false
);
CREATE TEMP TABLE product_scope (LIKE product_type INCLUDING ALL);
CREATE TEMP TABLE country (
    id uuid PRIMARY KEY, name varchar NOT NULL, code varchar(2) NOT NULL,
    created_at timestamptz NOT NULL, updated_at timestamptz, is_deleted boolean NOT NULL DEFAULT false
);
CREATE TEMP TABLE product_variant (
    id uuid PRIMARY KEY, product_id uuid NOT NULL, volume real NOT NULL,
    price real NOT NULL, cost_price real NOT NULL, is_default boolean NOT NULL,
    is_deleted boolean NOT NULL, created_at timestamptz NOT NULL, updated_at timestamptz
);
CREATE TEMP TABLE product_category (id uuid PRIMARY KEY, product_id uuid NOT NULL, category_id uuid NOT NULL);
CREATE TEMP TABLE product_country (id uuid PRIMARY KEY, product_id uuid NOT NULL, country_id uuid NOT NULL);
CREATE TEMP TABLE product_product_scope (id uuid PRIMARY KEY, product_id uuid NOT NULL, product_scope_id uuid NOT NULL);

CREATE TYPE pg_temp.discount_type AS ENUM ('percentage', 'fixed');
CREATE TYPE pg_temp.target_type AS ENUM ('product', 'brand', 'category', 'all');
CREATE TEMP TABLE promotion (
    id uuid PRIMARY KEY, title varchar(255) NOT NULL, description text, discount_type pg_temp.discount_type NOT NULL,
    discount_value real NOT NULL, target_type pg_temp.target_type NOT NULL, target_ids uuid[],
    min_quantity integer, buy_quantity integer, get_quantity integer, priority integer,
    valid_from timestamptz NOT NULL, valid_until timestamptz NOT NULL, is_active boolean,
    created_at timestamptz, updated_at timestamptz
);
CREATE TEMP TABLE promo_code (
    id uuid PRIMARY KEY, code varchar(50) NOT NULL, discount_type pg_temp.discount_type NOT NULL,
    discount_value real NOT NULL, min_order_amount real, max_discount_amount real, usage_limit integer, used_count integer,
    valid_from timestamptz NOT NULL, valid_until timestamptz NOT NULL, is_active boolean, applicable_to uuid[],
    created_at timestamptz, updated_at timestamptz
);
CREATE TEMP TABLE product_promotion (product_id uuid, promotion_id uuid, PRIMARY KEY (product_id, promotion_id));
CREATE TEMP TABLE product_promo_code (product_id uuid, promo_code_id uuid, PRIMARY KEY (product_id, promo_code_id));
