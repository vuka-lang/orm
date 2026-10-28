SELECT `categories`.`id`, (COUNT(DISTINCT `products`.`id`)) AS `n` FROM `categories` LEFT JOIN `aggregate_products` AS `products` ON `products`.`aggregate_category_id` = `categories`.`id` GROUP BY `categories`.`id` HAVING (COUNT(DISTINCT `products`.`id`)) > ? ORDER BY (COUNT(DISTINCT `products`.`id`)) DESC;
-- args: [0]
