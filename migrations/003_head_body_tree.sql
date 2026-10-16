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
		IF to_regclass(format('%I.%I', 'body', table_name)) IS NULL THEN
			RAISE EXCEPTION 'Missing body.% table for head.%', table_name, table_name;
		END IF;

		EXECUTE format(
			'ALTER TABLE %I.%I
				ADD COLUMN IF NOT EXISTS custom_columns jsonb NOT NULL DEFAULT ''[]''::jsonb,
				ADD COLUMN IF NOT EXISTS hidden_columns jsonb NOT NULL DEFAULT ''[]''::jsonb',
			'head', table_name
		);
		EXECUTE format(
			'ALTER TABLE %I.%I
				ADD COLUMN IF NOT EXISTS sort_order integer NOT NULL DEFAULT 0,
				ADD COLUMN IF NOT EXISTS custom_fields jsonb NOT NULL DEFAULT ''{}''::jsonb',
			'body', table_name
		);

		EXECUTE format(
			'UPDATE %I.%I SET module_name = ''rem'' WHERE lower(btrim(module_name)) IN (''rem'', ''remark'')',
			'body', table_name
		);
		EXECUTE format(
			'WITH ordered AS (
				SELECT id, row_number() OVER (PARTITION BY module_name ORDER BY id)::integer - 1 AS next_order
				FROM %I.%I
			)
			UPDATE %I.%I AS item SET sort_order = ordered.next_order
			FROM ordered WHERE item.id = ordered.id',
			'body', table_name, 'body', table_name
		);

		EXECUTE format(
			'CREATE INDEX IF NOT EXISTS %I ON %I.%I (module_name, sort_order, id)',
			'ix_body_' || substr(md5(table_name), 1, 12) || '_module_order', 'body', table_name
		);
	END LOOP;
END;
$migration$;

COMMIT;
