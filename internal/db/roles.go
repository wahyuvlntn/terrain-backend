package db

import (
	"context"
	"fmt"
)

// Service roles of the agentic AI services (created NOLOGIN by migration
// 0005_agentic.sql).
const (
	// AIRole is used by the Python services: it reads plant data and owns
	// the rag / analytics / sim / agent schemas.
	AIRole = "afdd_ai"
	// AIReadOnlyRole is used by the MCP database server.
	AIReadOnlyRole = "afdd_ai_ro"
)

// EnsureServiceRoles enables login for the AI service roles with the given
// passwords (AI_DB_PASSWORD / AI_DB_RO_PASSWORD), so no secret lives in a
// migration. A role whose password is empty is left unchanged. It runs on
// every start-up, so a changed password takes effect with the next restart.
func EnsureServiceRoles(ctx context.Context, d *DB, rwPassword, roPassword string) error {
	return ensureRolePasswords(ctx, d, []rolePassword{{AIRole, rwPassword}, {AIReadOnlyRole, roPassword}})
}

type rolePassword struct {
	role     string
	password string
}

func ensureRolePasswords(ctx context.Context, q Querier, roles []rolePassword) error {
	for _, r := range roles {
		if r.password == "" {
			continue
		}
		// Check first: a failing ALTER ROLE would put the statement, password
		// included, into the server log (log_min_error_statement).
		var exists bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1)`, r.role).Scan(&exists); err != nil {
			return fmt.Errorf("look up role %s: %w", r.role, err)
		}
		if !exists {
			return fmt.Errorf("role %s does not exist (migration 0005_agentic creates it)", r.role)
		}
		// ALTER ROLE takes no bind parameters: let the server quote the
		// identifier and the password literal. The statement is never logged.
		var stmt string
		if err := q.QueryRow(ctx, `SELECT format('ALTER ROLE %I WITH LOGIN PASSWORD %L', $1::text, $2::text)`, r.role, r.password).
			Scan(&stmt); err != nil {
			return fmt.Errorf("build ALTER ROLE for %s: %w", r.role, err)
		}
		if _, err := q.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("set password of role %s: %w", r.role, err)
		}
	}
	return nil
}
