package control

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/barqdb/barq-server/internal/dataplane"
)

func TestBarqStoreCRUDVersionsAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "_system", "control.barq")
	store, err := OpenBarqStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	zero := uint64(0)
	created, err := store.Put(ctx, "webhooks", "hook-1", json.RawMessage(`{"enabled":true}`), &zero)
	if err != nil || created.Version != 1 {
		t.Fatalf("create: %+v %v", created, err)
	}
	if _, err := store.Put(ctx, "webhooks", "hook-1", json.RawMessage(`{}`), &zero); !dataplane.IsCode(err, dataplane.CodeConflict) {
		t.Fatalf("expected create conflict, got %v", err)
	}
	version := created.Version
	updated, err := store.Put(ctx, "webhooks", "hook-1", json.RawMessage(`{"enabled":false}`), &version)
	if err != nil || updated.Version != 2 {
		t.Fatalf("update: %+v %v", updated, err)
	}
	if _, err := store.Put(ctx, "webhooks", "hook-1", json.RawMessage(`{}`), &version); !dataplane.IsCode(err, dataplane.CodeConflict) {
		t.Fatalf("expected stale conflict, got %v", err)
	}
	if _, err := store.Put(ctx, "webhooks", "other", json.RawMessage(`{"enabled":true}`), &zero); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List(ctx, "webhooks", "hook")
	if err != nil || len(listed) != 1 || listed[0].Key != "hook-1" {
		t.Fatalf("list: %+v %v", listed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenBarqStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restarted, err := store.Get(ctx, "webhooks", "hook-1")
	if err != nil || restarted.Version != 2 || string(restarted.Value) != `{"enabled":false}` {
		t.Fatalf("restart read: %+v %v", restarted, err)
	}
	if err := store.Delete(ctx, "webhooks", "hook-1", &restarted.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "webhooks", "hook-1"); !dataplane.IsCode(err, dataplane.CodeNotFound) {
		t.Fatalf("expected not found after delete, got %v", err)
	}
}

func TestBarqStoreApplyIsAtomic(t *testing.T) {
	store, err := OpenBarqStore(filepath.Join(t.TempDir(), "control.barq"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	zero := uint64(0)
	if _, err := store.Put(ctx, "webhooks", "existing", json.RawMessage(`{"enabled":true}`), &zero); err != nil {
		t.Fatal(err)
	}

	_, err = store.Apply(ctx, []Mutation{
		{Op: MutationPut, Collection: "webhooks", Key: "new", Value: json.RawMessage(`{"enabled":true}`), ExpectedVersion: &zero},
		{Op: MutationPut, Collection: "webhooks", Key: "existing", Value: json.RawMessage(`{"enabled":false}`), ExpectedVersion: &zero},
	})
	if !dataplane.IsCode(err, dataplane.CodeConflict) {
		t.Fatalf("expected transaction conflict, got %v", err)
	}
	if _, err := store.Get(ctx, "webhooks", "new"); !dataplane.IsCode(err, dataplane.CodeNotFound) {
		t.Fatalf("first mutation leaked from failed transaction: %v", err)
	}
}
