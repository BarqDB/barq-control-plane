package webhooks_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/barqdb/barq-server/internal/control"
	"github.com/barqdb/barq-server/internal/dataplane"
	"github.com/barqdb/barq-server/internal/transforms"
	"github.com/barqdb/barq-server/internal/webhooks"
)

func TestWebhookEndToEndAndRecovery(t *testing.T) {
	ctx := context.Background()
	var gotPayload []byte
	var gotSignature, gotEventID string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPayload, _ = io.ReadAll(r.Body)
		gotSignature = r.Header.Get("X-Barq-Signature")
		gotEventID = r.Header.Get("X-Barq-Event-ID")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	scope := dataplane.Scope{Tenant: "tenant-a", Database: "main"}
	data := scriptedDataPlane(t, scope)
	storePath := filepath.Join(t.TempDir(), "control.barq")
	store, err := control.OpenBarqStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	runtime := transforms.NewQuickJS()
	service := webhooks.NewService(store, runtime, true)
	registered, err := service.Register(ctx, webhooks.Registration{
		Name: "paid order", Scope: scope, URL: target.URL, Events: []string{"Order.created"}, ObjectTypes: []string{"Order"},
		Reads: []dataplane.RelatedRead{{As: "customer", From: "Customer", One: true, Where: dataplane.Filter{Field: "id", Op: "eq", Value: map[string]any{"$ref": "after.customer_id"}}}},
		Transform: webhooks.TransformConfig{Language: "javascript", Source: `export function filter(ctx) { return ctx.after.status === "paid"; }
export function transform(ctx) { return {order_id: ctx.after.id, email: ctx.related.customer.email}; }`},
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := webhooks.NewDispatcher(data, store, runtime, target.Client())
	processed, err := dispatcher.PollOnce(ctx, scope)
	if err != nil || processed != 2 {
		t.Fatalf("poll failed: processed=%d err=%v", processed, err)
	}

	// Simulate a process restart after materialization but before delivery.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restartedStore, err := control.OpenBarqStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer restartedStore.Close()
	restartedDispatcher := webhooks.NewDispatcher(data, restartedStore, runtime, target.Client())
	delivered, err := restartedDispatcher.DeliverDue(ctx, 10)
	if err != nil || delivered != 1 {
		t.Fatalf("delivery after restart failed: delivered=%d err=%v", delivered, err)
	}
	if string(gotPayload) != `{"order_id":"o1","email":"buyer@example.com"}` {
		t.Fatalf("unexpected payload %s", gotPayload)
	}
	mac := hmac.New(sha256.New, []byte(registered.Secret))
	_, _ = mac.Write([]byte(gotEventID + "."))
	_, _ = mac.Write(gotPayload)
	wantSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSignature != wantSignature {
		t.Fatalf("bad signature: got %s want %s", gotSignature, wantSignature)
	}
	// Re-reading from the saved cursor must not create another delivery.
	processed, err = restartedDispatcher.PollOnce(ctx, scope)
	if err != nil || processed != 0 {
		t.Fatalf("duplicate poll was not idempotent: processed=%d err=%v", processed, err)
	}
	records, _ := restartedStore.List(ctx, webhooks.CollectionDeliveries, registered.Webhook.ID+"/")
	if len(records) != 1 {
		t.Fatalf("expected one durable delivery, got %d", len(records))
	}
	var delivery webhooks.Delivery
	_ = json.Unmarshal(records[0].Value, &delivery)
	if delivery.Status != "completed" {
		t.Fatalf("delivery did not complete: %+v", delivery)
	}
}

func scriptedDataPlane(t *testing.T, scope dataplane.Scope) dataplane.DataPlane {
	t.Helper()
	committed := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	events := []dataplane.ChangeEvent{
		{ID: "tenant-a/main:1", Scope: scope, Cursor: 1, Snapshot: 1, Type: "Customer.created", ObjectType: "Customer", PrimaryKey: "c1", Source: "api", After: map[string]any{"id": "c1", "email": "buyer@example.com"}, CommittedAt: committed},
		{ID: "tenant-a/main:2", Scope: scope, Cursor: 2, Snapshot: 2, Type: "Order.created", ObjectType: "Order", PrimaryKey: "o1", Source: "sync", After: map[string]any{"id": "o1", "customer_id": "c1", "status": "paid"}, CommittedAt: committed},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/v1/changes":
			after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
			result := dataplane.ChangesResult{NextCursor: after}
			for _, event := range events {
				if event.Cursor > after {
					result.Events = append(result.Events, event)
					result.NextCursor = event.Cursor
				}
			}
			_ = json.NewEncoder(w).Encode(result)
		case "/internal/v1/events/materialize":
			var request dataplane.MaterializeRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			for _, event := range events {
				if event.ID == request.EventID {
					context := dataplane.EventContext{Event: event, After: event.After, Related: map[string]any{}}
					if event.ObjectType == "Order" {
						context.Related["customer"] = map[string]any{"id": "c1", "email": "buyer@example.com"}
					}
					_ = json.NewEncoder(w).Encode(context)
					return
				}
			}
			http.Error(w, `{"code":"not_found","message":"event not found"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"code":"not_found","message":"route not found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	data, err := dataplane.NewHTTPDataPlane(server.URL, "test", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return data
}
