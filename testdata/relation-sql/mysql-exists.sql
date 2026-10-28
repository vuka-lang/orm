SELECT `categories`.`id`, (EXISTS (SELECT 1 FROM `categories` AS `orm_rel_1` LEFT JOIN `aggregate_products` AS `products` ON `products`.`aggregate_category_id` = `orm_rel_1`.`id` WHERE `orm_rel_1`.`id` = `categories`.`id` AND (`products`.`status` = ?) AND `products`.`id` IS NOT NULL)) AS `has` FROM `categories`;
-- args: [2]
