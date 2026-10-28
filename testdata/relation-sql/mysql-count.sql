SELECT `categories`.`id`, `categories`.`name`, (COUNT(DISTINCT CASE WHEN `products`.`status` BETWEEN ? AND ? THEN `products`.`id` END)) AS `n` FROM `categories` LEFT JOIN `aggregate_products` AS `products` ON `products`.`aggregate_category_id` = `categories`.`id` GROUP BY `categories`.`id`, 2;
-- args: [2 3]
