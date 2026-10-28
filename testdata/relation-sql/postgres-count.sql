SELECT "categories"."id", "categories"."name", (COUNT(DISTINCT "products"."id") FILTER (WHERE "products"."status" BETWEEN $1 AND $2)) AS "n" FROM "categories" LEFT JOIN "aggregate_products" AS "products" ON "products"."aggregate_category_id" = "categories"."id" GROUP BY "categories"."id", 2;
-- args: [2 3]
