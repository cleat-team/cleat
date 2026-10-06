-- cleat#3171. See migrations/postgres/014 for the mechanism -- this is
-- signal_seq/signal_seq_at_claim's sibling (cleat#953) for the
-- promise/update wake paths rather than the signal one.
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS promise_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS promise_seq_at_claim BIGINT NOT NULL DEFAULT 0;
