-- 删除每张 head 鞋款表的 print_date 列。
-- Print Date 不再保存在数据库中，导出和打印时由前端自动生成当前日期。
-- 执行前请先备份数据库；本脚本幂等，可重复执行。

BEGIN;

DO $migration$
DECLARE
	table_name text;
BEGIN
	FOR table_name IN
		SELECT t.table_name
		FROM information_schema.tables t
		WHERE t.table_schema = 'head' AND t.table_type = 'BASE TABLE'
		ORDER BY t.table_name
	LOOP
		EXECUTE format(
			'ALTER TABLE %I.%I DROP COLUMN IF EXISTS print_date',
			'head', table_name
		);
	END LOOP;
END;
$migration$;

COMMIT;
