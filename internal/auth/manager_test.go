package auth_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/barqdb/barq-server/internal/auth"
	"github.com/barqdb/barq-server/internal/control"
	"github.com/barqdb/barq-server/internal/dataplane"
)

func TestBootstrapPersistsDigestsAndTenantScopes(t *testing.T) {
	manager, store := testManager(t)
	if err := manager.Bootstrap(t.Context(), auth.BootstrapOptions{
		APIKeys: "first-secret:*:*:*", DefaultTenant: "acme", DefaultDatabase: "main",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.FindByDigest(t.Context(), auth.Digest("first-secret")); err != nil {
		t.Fatalf("bootstrap key not stored: %v", err)
	}
	scopes, err := manager.Scopes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || scopes[0] != (dataplane.Scope{Tenant: "acme", Database: "main"}) {
		t.Fatalf("unexpected bootstrap scopes: %+v", scopes)
	}

	databases := []string{"audit"}
	if _, err := manager.UpdateTenant(t.Context(), "acme", auth.UpdateTenantInput{Databases: &databases}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Bootstrap(t.Context(), auth.BootstrapOptions{APIKeys: "replacement:*:*:*", DefaultTenant: "acme", DefaultDatabase: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.FindByDigest(t.Context(), auth.Digest("replacement")); !dataplane.IsCode(err, dataplane.CodeUnauthorized) {
		t.Fatalf("restart reapplied environment key: %v", err)
	}
	tenant, err := manager.GetTenant(t.Context(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(tenant.Databases) != 1 || tenant.Databases[0] != "audit" {
		t.Fatalf("restart reapplied environment tenant defaults: %+v", tenant.Databases)
	}
	records, err := store.List(t.Context(), "api_keys", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if bytes.Contains(record.Value, []byte("first-secret")) {
			t.Fatal("raw bootstrap secret was stored in control.barq")
		}
	}
}

func TestTenantStateControlsScopedKeys(t *testing.T) {
	manager, _ := testManager(t)
	bootstrap(t, manager)
	if _, err := manager.CreateTenant(t.Context(), auth.CreateTenantInput{ID: "client-b", Name: "Client B", Databases: []string{"main", "audit"}}); err != nil {
		t.Fatal(err)
	}
	created, err := manager.CreateKey(t.Context(), auth.CreateServiceKeyInput{
		Label: "orders", Tenant: "client-b", Database: "main", Actions: []string{"read", "write"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.FindByDigest(t.Context(), auth.Digest(created.Secret)); err != nil {
		t.Fatalf("new key rejected: %v", err)
	}
	databases := []string{"audit"}
	if _, err := manager.UpdateTenant(t.Context(), "client-b", auth.UpdateTenantInput{Databases: &databases}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.FindByDigest(t.Context(), auth.Digest(created.Secret)); !dataplane.IsCode(err, dataplane.CodeUnauthorized) {
		t.Fatalf("key for removed database still works: %v", err)
	}
	databases = []string{"main", "audit"}
	if _, err := manager.UpdateTenant(t.Context(), "client-b", auth.UpdateTenantInput{Databases: &databases}); err != nil {
		t.Fatal(err)
	}
	if err := manager.DisableTenant(t.Context(), "client-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.FindByDigest(t.Context(), auth.Digest(created.Secret)); !dataplane.IsCode(err, dataplane.CodeUnauthorized) {
		t.Fatalf("disabled tenant key still works: %v", err)
	}
}

func TestBootstrapRecoversMissingTenantRegistry(t *testing.T) {
	manager, store := testManager(t)
	now := time.Now().UTC()
	stored := auth.ServiceKey{
		ID: "key_existing", Digest: auth.Digest("existing-secret"), Tenant: "*", Database: "*",
		Actions: []string{"*"}, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	value, err := control.Encode(stored)
	if err != nil {
		t.Fatal(err)
	}
	zero := uint64(0)
	if _, err := store.Put(t.Context(), "api_keys", stored.Digest, value, &zero); err != nil {
		t.Fatal(err)
	}

	if err := manager.Bootstrap(t.Context(), auth.BootstrapOptions{
		APIKeys: "replacement:*:*:*", DefaultTenant: "acme", DefaultDatabase: "main",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.GetTenant(t.Context(), "acme"); err != nil {
		t.Fatalf("missing tenant registry was not recovered: %v", err)
	}
	if _, err := manager.FindByDigest(t.Context(), auth.Digest("replacement")); !dataplane.IsCode(err, dataplane.CodeUnauthorized) {
		t.Fatalf("recovery reapplied the environment key: %v", err)
	}
}

func TestDevBootstrapCreatesGlobalAdminAndDefaultScope(t *testing.T) {
	manager, _ := testManager(t)
	if err := manager.Bootstrap(t.Context(), auth.BootstrapOptions{DevMode: true}); err != nil {
		t.Fatal(err)
	}
	key, err := manager.FindByDigest(t.Context(), auth.Digest("dev-key"))
	if err != nil {
		t.Fatal(err)
	}
	if key.Tenant != "*" || key.Database != "*" {
		t.Fatalf("development key is not a global admin: %+v", key)
	}
	scopes, err := manager.Scopes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || scopes[0] != (dataplane.Scope{Tenant: "dev", Database: "default"}) {
		t.Fatalf("unexpected development scopes: %+v", scopes)
	}
}

func TestRootKeyCannotBeLostAndRotationIsAtomic(t *testing.T) {
	manager, _ := testManager(t)
	bootstrap(t, manager)
	keys, err := manager.ListKeys(t.Context())
	if err != nil || len(keys) != 1 {
		t.Fatalf("bootstrap keys: %+v %v", keys, err)
	}
	if err := manager.RevokeKey(t.Context(), keys[0].ID); !dataplane.IsCode(err, dataplane.CodeConflict) {
		t.Fatalf("last root key was revocable: %v", err)
	}
	created, err := manager.CreateKey(t.Context(), auth.CreateServiceKeyInput{Label: "second root", Tenant: "*", Database: "*", Actions: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := manager.RotateKey(t.Context(), created.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Secret == created.Secret || rotated.Secret == "" {
		t.Fatal("rotation did not return a fresh secret")
	}
	if _, err := manager.FindByDigest(t.Context(), auth.Digest(created.Secret)); !dataplane.IsCode(err, dataplane.CodeUnauthorized) {
		t.Fatalf("old rotated key still works: %v", err)
	}
	if _, err := manager.FindByDigest(t.Context(), auth.Digest(rotated.Secret)); err != nil {
		t.Fatalf("new rotated key does not work: %v", err)
	}
}

func testManager(t *testing.T) (*auth.Manager, control.Store) {
	t.Helper()
	store, err := control.OpenBarqStore(t.TempDir() + "/control.barq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return auth.NewManager(store), store
}

func bootstrap(t *testing.T, manager *auth.Manager) {
	t.Helper()
	if err := manager.Bootstrap(context.Background(), auth.BootstrapOptions{APIKeys: "root-secret:*:*:*", DefaultTenant: "acme", DefaultDatabase: "main"}); err != nil {
		t.Fatal(err)
	}
}
