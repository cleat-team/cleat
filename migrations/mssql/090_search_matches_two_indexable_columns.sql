-- No-op on SQL Server, for the reason migration 033 gives: its full-text
-- search does not match arbitrary-substring LIKE semantics, so an index here
-- would change which rows come back rather than how fast they come back.
--
-- The accompanying Go change -- Search over two short text columns instead of
-- four including two JSON casts -- is what helps this dialect.
SELECT 1;
