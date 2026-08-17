package storage

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestPostgresChainTransferLifecycleMigrationUpgradesV250Database(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	migrationURL, admin, schema := postgresTestSchema(t, ctx, dsn, "chain_lifecycle_upgrade")
	if _, err := admin.Exec(ctx, `CREATE TABLE `+schema+`.schema_migrations(version TEXT PRIMARY KEY,applied_at TIMESTAMPTZ NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO `+schema+`.schema_migrations(version,applied_at) VALUES($1,NOW())`, broadcastMessagePreferencesMigrationVersion); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		store, err := Open(ctx, migrationURL)
		if err != nil {
			t.Fatalf("Open attempt %d: %v", attempt+1, err)
		}
		store.Close()
	}

	var marker, stateTable bool
	if err := admin.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM `+schema+`.schema_migrations WHERE version=$1),
		to_regclass($2) IS NOT NULL`,
		chainTransferLifecycleMigrationVersion, schema+".chain_transfer_states").Scan(&marker, &stateTable); err != nil {
		t.Fatal(err)
	}
	if !marker || !stateTable {
		t.Fatalf("migration marker=%t state_table=%t", marker, stateTable)
	}
}
