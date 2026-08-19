package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestEnsureRolePasswords needs AFDD_TEST_DATABASE_URL (a superuser on a
// test server). It works on a scratch database and a scratch role, never on
// the real service roles.
func TestEnsureRolePasswords(t *testing.T) {
	base := os.Getenv("AFDD_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("AFDD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	dbName := fmt.Sprintf("afdd_roles_test_%d", suffix)
	role := fmt.Sprintf("afdd_roles_test_%d", suffix)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = admin.Exec(c, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		_, _ = admin.Exec(c, "DROP ROLE IF EXISTS "+role)
		admin.Close(c)
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + dbName
	d, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	canLogin := func() bool {
		var ok bool
		if err := d.QueryRow(ctx, `SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, role).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// An empty password leaves the role alone.
	if err := ensureRolePasswords(ctx, d, []rolePassword{{role, ""}}); err != nil {
		t.Fatal(err)
	}
	if canLogin() {
		t.Fatal("empty password enabled login")
	}

	// Quotes, backslashes and a dollar sign must survive the quoting.
	password := `it's a "test" \ pa$$word`
	for i := 0; i < 2; i++ { // idempotent
		if err := ensureRolePasswords(ctx, d, []rolePassword{{role, password}}); err != nil {
			t.Fatal(err)
		}
	}
	if !canLogin() {
		t.Fatal("login not enabled")
	}
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password = role, password
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("login with the new password: %v", err)
	}
	var who string
	if err := conn.QueryRow(ctx, `SELECT current_user`).Scan(&who); err != nil || who != role {
		t.Errorf("current_user = %q (%v)", who, err)
	}
	conn.Close(ctx)

	// A missing role is an error that does not reveal the password.
	err = ensureRolePasswords(ctx, d, []rolePassword{{role + "_missing", "secret-value-123"}})
	if err == nil || strings.Contains(err.Error(), "secret-value-123") {
		t.Errorf("missing role: %v", err)
	}
}
