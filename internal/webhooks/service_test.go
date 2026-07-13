package webhooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/barqdb/barq-server/internal/control"
	"github.com/barqdb/barq-server/internal/dataplane"
	"github.com/barqdb/barq-server/internal/transforms"
)

func TestWebhookRevisionRotationAndReplay(t *testing.T) {
	ctx := context.Background()
	store := openServiceStore(t)
	runtime := transforms.NewQuickJS()
	service := NewService(store, runtime, true)
	input := Registration{
		Name: "tasks", Scope: dataplane.Scope{Tenant: "a", Database: "main"}, URL: "http://127.0.0.1/hook",
		Events: []string{"Task.created"}, Transform: TransformConfig{Language: "javascript", Source: `function transform(ctx) { return ctx.after; }`},
	}
	registered, err := service.Register(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	input.Name = "tasks-v2"
	updated, err := service.Update(ctx, registered.Webhook.ID, input)
	if err != nil || updated.ActiveRevision != 2 {
		t.Fatalf("update failed: %+v %v", updated, err)
	}
	secret, err := service.RotateSecret(ctx, updated.ID)
	if err != nil || secret == "" || secret == registered.Secret {
		t.Fatalf("rotation failed: %q %v", secret, err)
	}
	afterRotation, _ := service.Get(ctx, updated.ID)
	if afterRotation.ActiveRevision != 3 {
		t.Fatalf("expected third immutable revision: %+v", afterRotation)
	}
	dead := Delivery{ID: deliveryID(updated.ID, 3, "event-1"), WebhookID: updated.ID, Revision: 3, EventID: "event-1", Status: "dead", Stage: "delivery"}
	encoded, _ := control.Encode(dead)
	zero := uint64(0)
	_, _ = store.Put(ctx, CollectionDeliveries, dead.ID, encoded, &zero)
	count, err := service.Replay(ctx, updated.ID)
	if err != nil || count != 1 {
		t.Fatalf("replay failed: %d %v", count, err)
	}
}

func TestDeliveryRetriesBecomeDeadLetter(t *testing.T) {
	ctx := context.Background()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusBadGateway) }))
	defer target.Close()
	data, err := dataplane.NewHTTPDataPlane("http://127.0.0.1:1", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	store := openServiceStore(t)
	runtime := transforms.NewQuickJS()
	service := NewService(store, runtime, true)
	scope := dataplane.Scope{Tenant: "a", Database: "main"}
	registered, err := service.Register(ctx, Registration{
		Name: "failing", Scope: scope, URL: target.URL, Events: []string{"Task.created"},
		Transform: TransformConfig{Language: "javascript", Source: `function transform(ctx) { return ctx.after; }`},
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(data, store, runtime, target.Client())
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	dispatcher.now = func() time.Time { return now }
	delivery := Delivery{
		ID:        deliveryID(registered.Webhook.ID, registered.Webhook.ActiveRevision, "event-1"),
		WebhookID: registered.Webhook.ID, Revision: registered.Webhook.ActiveRevision, EventID: "event-1",
		URL: target.URL, Payload: []byte(`{"id":"one"}`), Status: "pending", Stage: "delivery",
		NextAttemptAt: now, CreatedAt: now,
	}
	encoded, _ := control.Encode(delivery)
	zero := uint64(0)
	if _, err := store.Put(ctx, CollectionDeliveries, delivery.ID, encoded, &zero); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 8; attempt++ {
		if _, err := dispatcher.DeliverDue(ctx, 10); err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Hour)
	}
	records, _ := store.List(ctx, CollectionDeliveries, registered.Webhook.ID+"/")
	if len(records) != 1 {
		t.Fatalf("expected one delivery, got %d", len(records))
	}
	storedDelivery, _ := control.Decode[Delivery](records[0])
	if storedDelivery.Status != "dead" || storedDelivery.Attempts != 8 || storedDelivery.Stage != "delivery" {
		t.Fatalf("unexpected dead letter: %+v", storedDelivery)
	}
}

func openServiceStore(t *testing.T) *control.BarqStore {
	t.Helper()
	store, err := control.OpenBarqStore(t.TempDir() + "/control.barq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
