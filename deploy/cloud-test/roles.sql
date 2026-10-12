-- The login roles of the GCP deployment (stage 05 creates them on Cloud SQL with
-- deployment/gcp/db-tools/sql/roles.sql and generated passwords; these are throwaway local test values).
DO $roles$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'afdd_api') THEN
        CREATE ROLE afdd_api LOGIN PASSWORD 'cloudtest-api';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'afdd_ai') THEN
        CREATE ROLE afdd_ai LOGIN PASSWORD 'cloudtest-ai';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'afdd_ai_ro') THEN
        CREATE ROLE afdd_ai_ro LOGIN PASSWORD 'cloudtest-ai-ro';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'afdd_mcp_usage') THEN
        CREATE ROLE afdd_mcp_usage LOGIN PASSWORD 'cloudtest-mcp';
    END IF;
END
$roles$;
