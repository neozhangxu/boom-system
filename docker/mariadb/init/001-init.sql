CREATE DATABASE IF NOT EXISTS `bom_header`
  CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE IF NOT EXISTS `bom_parts`
  CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE IF NOT EXISTS `bom_item`
  CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE IF NOT EXISTS `bom_colorway`
  CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE IF NOT EXISTS `bom_item_color_detail`
  CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `bom_header`.`bom_header` (
  `id` INT NOT NULL AUTO_INCREMENT,
  `company_name` VARCHAR(200) NOT NULL DEFAULT '',
  `document_title` VARCHAR(200) NOT NULL DEFAULT '',
  `sample_order_no` VARCHAR(200) NOT NULL DEFAULT '',
  `sample_type` VARCHAR(200) NOT NULL DEFAULT '',
  `customer` VARCHAR(200) NOT NULL DEFAULT '',
  `season` VARCHAR(200) NOT NULL DEFAULT '',
  `brand` VARCHAR(200) NOT NULL DEFAULT '',
  `gender` VARCHAR(200) NOT NULL DEFAULT '',
  `style_no` VARCHAR(64) NULL,
  `style_name` VARCHAR(200) NOT NULL DEFAULT '',
  `last_no` VARCHAR(200) NOT NULL DEFAULT '',
  `bottom_no` VARCHAR(200) NOT NULL DEFAULT '',
  `stage` TEXT NULL,
  `request_finish_date` VARCHAR(50) NULL,
  `pattern_master` VARCHAR(200) NULL,
  `tooling_master` VARCHAR(200) NULL,
  `shoe_filler_no` VARCHAR(100) NULL,
  `approved_by` VARCHAR(200) NOT NULL DEFAULT '',
  `reviewed_by` VARCHAR(200) NOT NULL DEFAULT '',
  `prepared_by` VARCHAR(200) NOT NULL DEFAULT '',
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
