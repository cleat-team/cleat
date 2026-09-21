-- cleat migration 095 (postgres): an org suspension column with no reader
--
-- cleat#1943. admin.orgs carried a `suspended` column with no reader and no
-- writer. It was added in 091 mirroring admin.tenants' shape, but org
-- suspension was never built: tenant suspension (the #1915 path, reading
-- admin.tenants.suspended) is separate and wired, while an org marked
-- suspended still runs every one of its tenants. The column is the "mechanism
-- wired to nothing" shape -- it reads as "org suspension exists" while doing
-- nothing -- and it contradicts 091's own header, which says admin.orgs
-- "CARRIES IDENTITY ONLY ... policy should not be built in there".
--
-- If org suspension is ever built it is a separate feature, and the column
-- returns WITH its reader -- the rule 092 recorded for idx_instances_input_gin
-- ("comes back WITH its reader, and then it is not speculative").

ALTER TABLE admin.orgs DROP COLUMN IF EXISTS suspended;
