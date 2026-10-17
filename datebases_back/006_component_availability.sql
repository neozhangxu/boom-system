BEGIN;

DO $migration$
DECLARE
	table_name text;
	default_components jsonb := '["upper","lining","reinforcement","sock_liner","insole","outsole","accessories","packing","stitching","rem"]'::jsonb;
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
				ADD COLUMN IF NOT EXISTS components jsonb NOT NULL DEFAULT ''[]''::jsonb',
			'head', table_name
		);
		EXECUTE format(
			'ALTER TABLE %I.%I ALTER COLUMN components SET DEFAULT ''[]''::jsonb',
			'head', table_name
		);
		EXECUTE format(
			'UPDATE %I.%I AS header
			SET components = COALESCE((
				SELECT jsonb_agg(allowed.category ORDER BY allowed.ordinal)
				FROM unnest(ARRAY[
					''upper'', ''lining'', ''reinforcement'', ''sock_liner'', ''insole'',
					''outsole'', ''accessories'', ''packing'', ''stitching'', ''rem''
				]::text[]) WITH ORDINALITY AS allowed(category, ordinal)
				WHERE EXISTS (
					SELECT 1 FROM %I.%I AS item
					WHERE lower(btrim(item.module_name)) = allowed.category
				)
			), ''[]''::jsonb)
			WHERE header.components = $1 OR header.components = ''[]''::jsonb',
			'head', table_name, 'body', table_name
		) USING default_components;
	END LOOP;
END;
$migration$;

COMMIT;
