-- Check for orphaned concurrency_keys rows before adding the FK
SELECT COUNT(*) AS orphaned_concurrency_keys FROM concurrency_keys ck LEFT JOIN workflow_instances wi ON ck.workflow_id = wi.id WHERE wi.id IS NULL;

DELETE ck FROM concurrency_keys ck LEFT JOIN workflow_instances wi ON ck.workflow_id = wi.id WHERE wi.id IS NULL;

-- event_history: replace existing FK with CASCADE
SET @cname = (SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE TABLE_NAME = 'event_history' AND CONSTRAINT_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME = 'workflow_instances');
SET @sql = CONCAT('ALTER TABLE event_history DROP FOREIGN KEY ', @cname);
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
ALTER TABLE event_history ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE;

-- workflow_signals: replace existing FK with CASCADE
SET @cname = (SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE TABLE_NAME = 'workflow_signals' AND CONSTRAINT_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME = 'workflow_instances');
SET @sql = CONCAT('ALTER TABLE workflow_signals DROP FOREIGN KEY ', @cname);
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
ALTER TABLE workflow_signals ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE;

-- workflow_promises: replace existing FK with CASCADE
SET @cname = (SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE TABLE_NAME = 'workflow_promises' AND CONSTRAINT_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME = 'workflow_instances');
SET @sql = CONCAT('ALTER TABLE workflow_promises DROP FOREIGN KEY ', @cname);
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
ALTER TABLE workflow_promises ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE;

-- workflow_update_requests: replace existing FK with CASCADE
SET @cname = (SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE TABLE_NAME = 'workflow_update_requests' AND CONSTRAINT_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME = 'workflow_instances');
SET @sql = CONCAT('ALTER TABLE workflow_update_requests DROP FOREIGN KEY ', @cname);
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
ALTER TABLE workflow_update_requests ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE;

-- concurrency_keys: ADD the missing FK (was never defined in MySQL 001_tables.sql)
ALTER TABLE concurrency_keys ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE;
