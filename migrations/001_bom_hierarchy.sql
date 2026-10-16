BEGIN;

DO $migration$
DECLARE
    category_schema TEXT;
    relation_name TEXT;
    id_type TEXT;
    index_name TEXT;
    sort_order_missing BOOLEAN;
    category_schemas TEXT[] := ARRAY[
        'upper', 'stitching', 'accessories', 'insole', 'lining',
        'outsole', 'packing', 'reinforcement', 'sock_liner'
    ];
BEGIN
    FOREACH category_schema IN ARRAY category_schemas LOOP
        FOR relation_name IN
            SELECT table_name
            FROM information_schema.tables
            WHERE table_schema = category_schema AND table_type = 'BASE TABLE'
        LOOP
            SELECT format_type(attribute.atttypid, attribute.atttypmod)
            INTO id_type
            FROM pg_attribute AS attribute
            WHERE attribute.attrelid = format('%I.%I', category_schema, relation_name)::regclass
                AND attribute.attname = 'id' AND NOT attribute.attisdropped;

            IF id_type IS NULL THEN
                RAISE EXCEPTION 'Missing id column in %.%', category_schema, relation_name;
            END IF;

            SELECT NOT EXISTS (
                SELECT 1 FROM information_schema.columns
                WHERE table_schema = category_schema AND table_name = relation_name
                    AND column_name = 'sort_order'
            )
            INTO sort_order_missing;

            EXECUTE format(
                'ALTER TABLE %I.%I ADD COLUMN IF NOT EXISTS parent_id %s',
                category_schema, relation_name, id_type
            );
            EXECUTE format(
                'ALTER TABLE %I.%I ADD COLUMN IF NOT EXISTS sort_order INTEGER NOT NULL DEFAULT 0',
                category_schema, relation_name
            );
            IF sort_order_missing THEN
                EXECUTE format(
                    'UPDATE %I.%I AS item SET sort_order = ranked.position
                     FROM (
                         SELECT id, row_number() OVER (PARTITION BY style_no, parent_id ORDER BY id)::integer - 1 AS position
                         FROM %I.%I
                     ) AS ranked
                     WHERE item.id = ranked.id',
                    category_schema, relation_name, category_schema, relation_name
                );
            END IF;

            IF NOT EXISTS (
                SELECT 1
                FROM pg_constraint AS constraint_row
                JOIN pg_class AS relation ON relation.oid = constraint_row.conrelid
                JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
                WHERE namespace.nspname = category_schema
                    AND relation.relname = relation_name
                    AND constraint_row.conname = 'fk_bom_parent'
                    AND constraint_row.contype = 'f'
            ) THEN
                EXECUTE format(
                    'ALTER TABLE %I.%I ADD CONSTRAINT fk_bom_parent
                     FOREIGN KEY (parent_id) REFERENCES %I.%I (id) ON DELETE CASCADE',
                    category_schema, relation_name, category_schema, relation_name
                );
            END IF;

            index_name := 'ix_bom_' || substr(md5(relation_name), 1, 12) || '_parent_sort';
            EXECUTE format(
                'CREATE INDEX IF NOT EXISTS %I ON %I.%I (style_no, parent_id, sort_order)',
                index_name, category_schema, relation_name
            );
        END LOOP;
    END LOOP;
END
$migration$;

COMMIT;
