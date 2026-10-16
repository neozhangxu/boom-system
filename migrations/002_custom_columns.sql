BEGIN;

DO $migration$
DECLARE
    relation_name TEXT;
    category_schema TEXT;
    category_schemas TEXT[] := ARRAY[
        'upper', 'stitching', 'accessories', 'insole', 'lining',
        'outsole', 'packing', 'reinforcement', 'sock_liner'
    ];
BEGIN
    FOR relation_name IN
        SELECT table_name
        FROM information_schema.tables
        WHERE table_schema = 'style' AND table_type = 'BASE TABLE'
    LOOP
        EXECUTE format(
            'ALTER TABLE style.%I ADD COLUMN IF NOT EXISTS custom_columns JSONB NOT NULL DEFAULT ''[]''::jsonb',
            relation_name
        );
        EXECUTE format(
            'ALTER TABLE style.%I ADD COLUMN IF NOT EXISTS hidden_columns JSONB NOT NULL DEFAULT ''[]''::jsonb',
            relation_name
        );
    END LOOP;

    FOREACH category_schema IN ARRAY category_schemas LOOP
        FOR relation_name IN
            SELECT table_name
            FROM information_schema.tables
            WHERE table_schema = category_schema AND table_type = 'BASE TABLE'
        LOOP
            EXECUTE format(
                'ALTER TABLE %I.%I ADD COLUMN IF NOT EXISTS custom_fields JSONB NOT NULL DEFAULT ''{}''::jsonb',
                category_schema, relation_name
            );
        END LOOP;
    END LOOP;
END
$migration$;

COMMIT;
