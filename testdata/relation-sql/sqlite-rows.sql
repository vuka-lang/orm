SELECT "categories"."id", (COUNT(*)) AS "n" FROM "categories" LEFT JOIN "aggregate_products" AS "products" ON "products"."aggregate_category_id" = "categories"."id" GROUP BY "categories"."id";
-- args: []
