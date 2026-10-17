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
				ADD COLUMN IF NOT EXISTS components jsonb NOT NULL
				DEFAULT ''[]''::jsonb',
			'head', table_name
		);
	END LOOP;
END;
$migration$;

COMMIT;
