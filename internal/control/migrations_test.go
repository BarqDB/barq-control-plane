package control

import (
	"context"
	"path/filepath"
	"testing"
)

func TestEnsureAndApplyControlSchemaAreIdempotent(t *testing.T) {
	store, err := OpenBarqStore(filepath.Join(t.TempDir(), "control.barq"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := EnsureSchema(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigration(ctx, store, 1, 1); err != nil {
		t.Fatal(err)
	}
	record, err := store.Get(ctx, systemCollection, schemaStateKey)
	if err != nil || record.Version != 1 {
		t.Fatalf("unexpected schema state: %+v %v", record, err)
	}
}

func TestControlMigrationRejectsUnsupportedOrDowngrade(t *testing.T) {
	if err := CheckMigration(1, 2); err == nil {
		t.Fatal("unsupported future schema was accepted")
	}
	if err := CheckMigration(2, 1); err == nil {
		t.Fatal("schema downgrade was accepted")
	}
}
