-- cleat migration 078 (mysql): an org groups a customer's tenants
--
-- cleat#1898. See migrations/postgres/091_an_org_groups_a_customers_tenants.sql
-- for the full reasoning: admin.orgs carries identity only (no plan, limit or
-- usage column -- billing systems key on org_id from outside), org_id is
-- immutable by trigger rather than by convention, and existing tenants get a
-- default org rather than a nullable sentinel.
--
-- NO admin SCHEMA HERE. MySQL has no schema separate from database --
-- tenant_store.go's own comment on this file's neighbours already states it:
-- "admin.tenant_api_keys" reads as a database named admin and fails with
-- "Unknown database 'admin'". orgs and the tenants table it references are
-- both unqualified, matching 001_schema.sql's own `tenants` (not
-- `admin.tenants`).
--
-- CHAR(36) for org_id, matching tenants.tenant_id's own type exactly -- not a
-- new representation for a UUID in a file that already has one.

CREATE TABLE IF NOT EXISTS orgs (
    org_id     CHAR(36)     NOT NULL,
    name       VARCHAR(255) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    suspended  TINYINT(1)   NOT NULL DEFAULT 0,
    UNIQUE KEY uq_orgs_name (name),
    PRIMARY KEY (org_id)
) ENGINE=InnoDB;

INSERT INTO orgs (org_id, name)
    VALUES ('00000000-0000-0000-0000-000000000000', 'default')
    ON DUPLICATE KEY UPDATE org_id = org_id;

-- MySQL has no ADD COLUMN IF NOT EXISTS -- the comment claiming otherwise on
-- an earlier draft of this file was checked against nothing and was wrong;
-- every other migration in this dialect already says so and uses the
-- prepared-statement idiom below, which this now matches.
SET @add_org_id := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenants ADD COLUMN org_id CHAR(36) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenants'
      AND COLUMN_NAME = 'org_id'
);
PREPARE add_org_id FROM @add_org_id; EXECUTE add_org_id; DEALLOCATE PREPARE add_org_id;

UPDATE tenants SET org_id = '00000000-0000-0000-0000-000000000000' WHERE org_id IS NULL;

-- DEFAULT, not just NOT NULL -- see the postgres migration's comment at the
-- same step. Without it, a direct INSERT into tenants that does not name
-- org_id fails, and roughly a dozen call sites across engine/*_test.go do
-- exactly that.
ALTER TABLE tenants MODIFY COLUMN org_id CHAR(36) NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000';

-- Guarded the same way 034's cleat_drop_defs_fks is: MySQL raises
-- ER_DUP_KEYNAME/ER_FK_DUP_NAME on a re-add rather than silently
-- no-op-ing, so this checks information_schema before adding rather than
-- relying on a form of ADD CONSTRAINT IF NOT EXISTS that does not exist.
SET @add_org_fk := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenants ADD CONSTRAINT tenants_org_id_fk FOREIGN KEY (org_id) REFERENCES orgs(org_id)',
        'DO 0')
    FROM information_schema.TABLE_CONSTRAINTS
    WHERE CONSTRAINT_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenants'
      AND CONSTRAINT_NAME = 'tenants_org_id_fk'
);
PREPARE add_org_fk FROM @add_org_fk; EXECUTE add_org_fk; DEALLOCATE PREPARE add_org_fk;

-- org_id IS IMMUTABLE. A BEFORE UPDATE trigger, not a comment: SIGNAL raises
-- an error the driver surfaces to the caller, and it fires regardless of
-- which account issues the UPDATE -- unlike a REVOKEd privilege, which the
-- table owner or an account with the grant would bypass entirely.
DROP TRIGGER IF EXISTS tenants_org_id_immutable;
DELIMITER //
CREATE TRIGGER tenants_org_id_immutable
BEFORE UPDATE ON tenants
FOR EACH ROW
BEGIN
    IF NEW.org_id <> OLD.org_id THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'tenants.org_id is immutable and cannot be changed';
    END IF;
END//
DELIMITER ;
