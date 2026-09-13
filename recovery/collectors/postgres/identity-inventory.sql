-- Explicitly run with psql -X -w -qAt -v ON_ERROR_STOP=1 against the authorized DB.
-- Catalog metadata only. No business rows, default expressions, function bodies,
-- role passwords, query text, environment settings or sequence current values.
BEGIN READ ONLY;
SET LOCAL statement_timeout = '3s';
SET LOCAL lock_timeout = '1s';
SET LOCAL search_path = pg_catalog;

SELECT jsonb_build_object(
  'schema_version', 1,
  'kind', 'portunex.identity.catalog_inventory',
  'collected_at', transaction_timestamp(),
  'transaction_read_only', current_setting('transaction_read_only'),
  'database', current_database(),
  'server_version', current_setting('server_version'),
  'scope', jsonb_build_object(
    'schema', 'public',
    'tables', jsonb_build_array('api_keys', 'auth_sessions', 'users'),
    'business_rows_read', false,
    'expressions_read', false,
    'reconstructable_schema', false
  ),
  'public_counts', jsonb_build_object(
    'tables', (SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')),
    'columns', (SELECT count(*) FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid = a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped),
    'foreign_keys', (SELECT count(*) FROM pg_catalog.pg_constraint k JOIN pg_catalog.pg_namespace n ON n.oid = k.connamespace WHERE n.nspname = 'public' AND k.contype = 'f'),
    'indexes', (SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relkind IN ('i', 'I')),
    'routines', (SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'public'),
    'user_triggers', (SELECT count(*) FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid = t.tgrelid JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND NOT t.tgisinternal),
    'policies', (SELECT count(*) FROM pg_catalog.pg_policy p JOIN pg_catalog.pg_class c ON c.oid = p.polrelid JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public')
  ),
  'columns', (SELECT jsonb_agg(jsonb_build_object(
    'table', c.relname, 'position', a.attnum, 'name', a.attname,
    'type', pg_catalog.format_type(a.atttypid, a.atttypmod),
    'column_not_null', a.attnotnull, 'has_default', a.atthasdef,
    'identity', a.attidentity, 'generated', a.attgenerated
  ) ORDER BY c.relname, a.attnum)
    FROM pg_catalog.pg_attribute a
    JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname IN ('api_keys', 'auth_sessions', 'users')
      AND c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped)
);
ROLLBACK;
