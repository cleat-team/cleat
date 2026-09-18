-- Search used to OR four columns: input, result, error_msg and def_name.
--
-- A disjunction is only as indexable as its WORST branch, and input/result are
-- JSONB cast to text, which no index covers. So every Search forced a
-- sequential scan over the whole filter -- and dragged error_msg down with it,
-- even though migration 033 gave error_msg a pg_trgm GIN index. That index has
-- been maintained on every write since, and could never be used from a Search.
--
-- Search now covers error_msg and def_name only; payload search moved to the
-- explicit input_contains and result_contains parameters. This gives def_name
-- the matching trigram index, so the planner can take a BitmapOr across both
-- branches rather than scanning.
--
-- WHY def_name IS CHEAP TO INDEX THIS WAY and input is not: def_name is short,
-- low-cardinality and written once per run, while input is an arbitrary JSON
-- payload rewritten on every update. 033 rejected a trigram index over
-- serialized JSON for exactly that write amplification, and that reasoning is
-- unchanged -- this is not the thin end of it.
--
-- The extension lookup is the shape 033 established: pg_trgm may live in any
-- schema, so the operator class is resolved rather than assumed.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

DO $def_name_trgm_index$
DECLARE
    ext_schema text;
BEGIN
    SELECT n.nspname INTO ext_schema
      FROM pg_extension e
      JOIN pg_namespace n ON n.oid = e.extnamespace
     WHERE e.extname = 'pg_trgm';

    IF ext_schema IS NULL THEN
        RAISE EXCEPTION 'pg_trgm is not installed and CREATE EXTENSION did not create it';
    END IF;

    EXECUTE format(
        'CREATE INDEX IF NOT EXISTS idx_instances_def_name_trgm '
        'ON workflow_instances USING GIN (def_name %I.gin_trgm_ops)',
        ext_schema);
END
$def_name_trgm_index$;
