package store

import "testing"

// Runs in the plain go job, so two PRs that each add the same version fail on
// their merged result instead of only at server start.
func TestEmbeddedMigrationsLoad(t *testing.T) {
	if _, err := loadMigrations(); err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
}
