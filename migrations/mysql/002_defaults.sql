-- cleat MySQL default data (002)
-- HAND-ASSEMBLED: a catalog dump carries no rows. cleat#2433.
--
-- ORDER IS LOAD-BEARING. tenants.org_id carries a foreign key to orgs(org_id)
-- (tenants_org_id_fk), and the tenant row's org_id defaults to the zero UUID --
-- so the ORG MUST EXIST FIRST. With the inserts the other way round, the
-- tenant insert fails 1452 and INSERT IGNORE turns that into a WARNING: the
-- statement reports success, the row silently does not land, and the
-- structural catalog diff is EMPTY because it does not see rows.
-- Measured 2026-09-26; this is the cleat#2059 PostgreSQL 002 defect in the
-- same shape.

-- The default org. Was migrations/mysql/078's own INSERT; folded here because
-- a dump carries no rows and 078 is deleted by the compaction.
INSERT INTO orgs (org_id, name)
    VALUES ('00000000-0000-0000-0000-000000000000', 'default')
    ON DUPLICATE KEY UPDATE org_id = org_id;

-- The default tenant. org_id is left to the column default, which is the same
-- zero UUID the chain's 078 backfilled it to.
INSERT IGNORE INTO tenants (tenant_id, name, display_name)
VALUES ('00000000-0000-0000-0000-000000000000', 'default', 'Default Tenant');
