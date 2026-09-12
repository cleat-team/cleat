-- cleat migration 064 (postgres): record how request/response were encoded,
-- instead of guessing it at read time.
--
-- tryDecodeBase64 base64-decoded a stored value and fell back to the raw string
-- when decoding FAILED. That is the wrong question: plenty of ordinary text
-- decodes successfully. StdEncoding accepts any string whose length is a
-- multiple of 4 and whose bytes are all in [A-Za-z0-9+/] with valid padding, so
-- EVERY four-character alphanumeric string decodes -- "test" became "\xb5\xeb-",
-- "user" became "\xba\xc7\xab", "true" became "\xb6\xbb\x9e". A legacy plaintext
-- value that happened to look like base64 was silently replaced with different
-- bytes (cleat#1319).
--
-- The information to answer it correctly was never stored. This stores it.
--
-- NULLABLE, AND NULL IS THE POINT. Three states:
--
--   NULL  the row predates this column; the encoding is genuinely unknown, so
--         reads keep the historical try-decode-and-fall-back behaviour
--   1     base64
--   0     plaintext (reserved; nothing writes it today)
--
-- So no backfill: a backfill would have to answer, for every existing row, the
-- exact question this column exists because nobody can answer. The ambiguity
-- stays confined to rows that are genuinely ambiguous, and every row written
-- from now on is unambiguous.
--
-- SMALLINT rather than BOOLEAN: same storage, and it leaves room for a future
-- encoding without a second migration on three dialects.
--
-- Note this is correct with --encrypt-sensitive-payloads too. EncryptString
-- stores base64(ciphertext), so the stored value is base64 either way; the read
-- path decodes and then decrypts.
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS payload_encoding SMALLINT;

COMMENT ON COLUMN event_history.payload_encoding IS
    'How request/response are encoded: NULL = unknown (pre-cleat#1319 row, decode is a guess), 1 = base64, 0 = plaintext.';
