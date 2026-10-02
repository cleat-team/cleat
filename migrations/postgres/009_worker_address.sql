-- cleat#2196. The reaper-to-worker veto channel needs a way to reach a
-- specific worker, and the owner's decision was DNS via a headless Service
-- (k8s/service.yaml, charts/cleat/templates/service.yaml), not a captured
-- pod IP. admin.workers.hostname is diagnostic only (os.Hostname(), not
-- necessarily routable); this is the address another worker can actually
-- dial, published only when --worker-service-name names a headless Service
-- that selects this pod (see cmd/cleat-worker/connection_share.go's
-- podAddress). Empty for every worker outside that configuration, which is
-- every worker today -- nothing yet reads this column (cleat#2196 step 4).
ALTER TABLE admin.workers ADD COLUMN IF NOT EXISTS address TEXT NOT NULL DEFAULT '';
