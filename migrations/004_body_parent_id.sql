BEGIN;

DO $migration$
DECLARE
	table_name text;
	id_type text;
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

		SELECT format_type(a.atttypid, a.atttypmod)
		INTO id_type
		FROM pg_attribute a
		WHERE a.attrelid = to_regclass(format('%I.%I', 'body', table_name))
			AND a.attname = 'id' AND NOT a.attisdropped;
		IF id_type IS NULL THEN
			RAISE EXCEPTION 'Missing body.%.id column', table_name;
		END IF;

		EXECUTE format(
			'ALTER TABLE %I.%I ADD COLUMN IF NOT EXISTS parent_id %s',
			'body', table_name, id_type
		);
		EXECUTE format(
			'CREATE UNIQUE INDEX IF NOT EXISTS %I ON %I.%I (id, module_name)',
			'ux_body_' || substr(md5(table_name), 1, 12) || '_id_module', 'body', table_name
		);
		EXECUTE format(
			'CREATE INDEX IF NOT EXISTS %I ON %I.%I (module_name, parent_id, sort_order, id)',
			'ix_body_' || substr(md5(table_name), 1, 12) || '_parent_order', 'body', table_name
		);
		IF NOT EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conrelid = to_regclass(format('%I.%I', 'body', table_name))
				AND conname = 'fk_bom_parent'
		) THEN
			EXECUTE format(
				'ALTER TABLE %I.%I ADD CONSTRAINT %I
					FOREIGN KEY (parent_id, module_name)
					REFERENCES %I.%I (id, module_name) ON DELETE CASCADE',
				'body', table_name, 'fk_bom_parent', 'body', table_name
			);
		END IF;
	END LOOP;
END;
$migration$;

COMMIT;
