-- Run only with psql -X -w -qAt -v ON_ERROR_STOP=1 against the authorized DB.
-- Catalog definitions for the three named tables and citext metadata only.
-- Expressions are deparsed, never evaluated; screen and review before saving.
-- No business rows, function bodies, role passwords or sequence current values.
BEGIN READ ONLY;
SET LOCAL statement_timeout = '3s';
SET LOCAL lock_timeout = '1s';
SET LOCAL search_path = pg_catalog;

SELECT jsonb_build_object(
  'schema_version', 1,
  'kind', 'portunex.identity.catalog_definition',
  'collected_at', transaction_timestamp(),
  'transaction_read_only', current_setting('transaction_read_only'),
  'database', current_database(),
  'server_version', current_setting('server_version'),
  'scope', jsonb_build_object(
    'schema', 'public',
    'tables', jsonb_build_array('api_keys', 'auth_sessions', 'users'),
    'extensions', jsonb_build_array('citext'),
    'business_rows_read', false,
    'expressions_read', true,
    'function_bodies_read', false,
    'sequence_current_values_read', false,
    'reconstructable_schema', false
  ),
  'columns', (SELECT coalesce(jsonb_agg(jsonb_build_object(
    'table', c.relname, 'position', a.attnum, 'name', a.attname,
    'type', pg_catalog.format_type(a.atttypid, a.atttypmod),
    'column_not_null', a.attnotnull, 'has_default', a.atthasdef,
    'identity', a.attidentity, 'generated', a.attgenerated,
    'default_expression', pg_catalog.pg_get_expr(d.adbin, d.adrelid, false),
    'collation_schema', cn.nspname, 'collation_name', co.collname
  ) ORDER BY c.relname, a.attnum), '[]'::jsonb)
    FROM pg_catalog.pg_attribute a
    JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
    LEFT JOIN pg_catalog.pg_collation co ON co.oid = a.attcollation
    LEFT JOIN pg_catalog.pg_namespace cn ON cn.oid = co.collnamespace
    WHERE n.nspname = 'public' AND c.relname IN ('api_keys', 'auth_sessions', 'users')
      AND c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped),
  'constraints', (SELECT coalesce(jsonb_agg(jsonb_build_object(
    'table', c.relname, 'name', k.conname, 'type', k.contype,
    'validated', k.convalidated, 'deferrable', k.condeferrable,
    'initially_deferred', k.condeferred, 'no_inherit', k.connoinherit,
    'column_positions', k.conkey,
    'referenced_schema', rn.nspname, 'referenced_table', rc.relname,
    'referenced_column_positions', k.confkey,
    'definition', pg_catalog.pg_get_constraintdef(k.oid, false)
  ) ORDER BY c.relname, k.conname), '[]'::jsonb)
    FROM pg_catalog.pg_constraint k
    JOIN pg_catalog.pg_class c ON c.oid = k.conrelid
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    LEFT JOIN pg_catalog.pg_class rc ON rc.oid = k.confrelid
    LEFT JOIN pg_catalog.pg_namespace rn ON rn.oid = rc.relnamespace
    WHERE n.nspname = 'public' AND c.relname IN ('api_keys', 'auth_sessions', 'users')
      AND c.relkind IN ('r', 'p')),
  'indexes', (SELECT coalesce(jsonb_agg(jsonb_build_object(
    'table', c.relname, 'name', ic.relname, 'method', am.amname,
    'unique', i.indisunique, 'primary', i.indisprimary,
    'valid', i.indisvalid, 'ready', i.indisready,
    'nulls_not_distinct', i.indnullsnotdistinct,
    'key_attribute_count', i.indnkeyatts, 'total_attribute_count', i.indnatts,
    'column_positions', i.indkey::smallint[],
    'definition', pg_catalog.pg_get_indexdef(i.indexrelid, 0, false),
    'predicate', pg_catalog.pg_get_expr(i.indpred, i.indrelid, false),
    'expressions', pg_catalog.pg_get_expr(i.indexprs, i.indrelid, false)
  ) ORDER BY c.relname, ic.relname), '[]'::jsonb)
    FROM pg_catalog.pg_index i
    JOIN pg_catalog.pg_class c ON c.oid = i.indrelid
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
    JOIN pg_catalog.pg_am am ON am.oid = ic.relam
    WHERE n.nspname = 'public' AND c.relname IN ('api_keys', 'auth_sessions', 'users')
      AND c.relkind IN ('r', 'p')),
  'extensions', (SELECT coalesce(jsonb_agg(jsonb_build_object(
    'name', e.extname, 'version', e.extversion,
    'schema', n.nspname, 'relocatable', e.extrelocatable
  ) ORDER BY e.extname), '[]'::jsonb)
    FROM pg_catalog.pg_extension e
    JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
    WHERE e.extname = 'citext')
);
ROLLBACK;
