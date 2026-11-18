GRANT USAGE ON SCHEMA public TO :"go_role";
GRANT SELECT ON
    public.banner, public.brand, public.category, public.country,
    public.product_scope, public.product_type, public.product, public.product_variant,
    public.product_category, public.product_country, public.product_product_scope,
    public.product_variant_price_history, public.product_promo_code,
    public.person, public."order", public.order_product, public.cart_add_operation,
    public.promotion, public.product_promotion, public.promo_code, public.promo_code_usage
    TO :"go_role";
GRANT INSERT, UPDATE, DELETE ON
    public.banner, public.brand, public.category, public.country,
    public.product_scope, public.product_type, public.product, public.product_variant,
    public.person, public.promotion, public.promo_code
    TO :"go_role";
GRANT INSERT, DELETE ON
    public.product_category, public.product_country, public.product_product_scope,
    public.order_product, public.product_promotion
    TO :"go_role";
GRANT INSERT, UPDATE ON
    public.product_variant_price_history, public.promo_code_usage, public."order"
    TO :"go_role";
GRANT INSERT ON public.cart_add_operation TO :"go_role";
