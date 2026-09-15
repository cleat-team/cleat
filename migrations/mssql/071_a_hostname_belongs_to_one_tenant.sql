-- cleat migration 071 (mssql): a hostname belongs to one tenant
--
-- cleat#1568. The PostgreSQL counterpart (079) carries the full reasoning; this
-- header records only what differs on SQL Server.
--
-- NO ROW-LEVEL SECURITY POLICY HERE. cleat's SQL Server isolation is carried by
-- explicit `AND tenant_id = @p` predicates in the statements themselves rather
-- than by a database policy -- engine/mssql_lifecycle.go's CountRunnableWorkflows
-- says so directly: "on SQL Server that predicate IS the whole of the tenant
-- scoping."
--
-- So the middleware's lookup MUST carry its own tenant predicate on this
-- dialect. It does, unconditionally and on every dialect, rather than only
-- where a policy is absent -- a query that is correct only because some other
-- layer is filtering is the shape of defect this repository has met repeatedly
-- (see the featureflags test asserting rows are scoped by a policy and not only
-- by the query). Carrying the predicate everywhere costs nothing and removes
-- the question.
--
-- NVARCHAR(255) rather than VARCHAR(MAX): SQL Server will not index a MAX
-- column, and this table exists to be looked up by hostname on every request.

IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'tenant_domains')
BEGIN
    CREATE TABLE tenant_domains (
        hostname   NVARCHAR(255)    NOT NULL,
        tenant_id  UNIQUEIDENTIFIER NOT NULL,
        created_at DATETIME2(6)     NOT NULL CONSTRAINT df_tenant_domains_created DEFAULT SYSUTCDATETIME(),

        CONSTRAINT pk_tenant_domains PRIMARY KEY (hostname),
        CONSTRAINT fk_tenant_domains_tenant FOREIGN KEY (tenant_id)
            REFERENCES tenants(tenant_id) ON DELETE CASCADE,
        CONSTRAINT ck_tenant_domains_hostname_lowercase
            CHECK (hostname = LOWER(hostname)),
        CONSTRAINT ck_tenant_domains_hostname_nonempty
            CHECK (LEN(hostname) > 0),
        CONSTRAINT ck_tenant_domains_hostname_no_port
            CHECK (CHARINDEX(':', hostname) = 0)
    );

    CREATE INDEX idx_tenant_domains_tenant ON tenant_domains(tenant_id);
END
