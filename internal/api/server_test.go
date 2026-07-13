package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/barqdb/barq-server/internal/api"
	"github.com/barqdb/barq-server/internal/auth"
	"github.com/barqdb/barq-server/internal/control"
	"github.com/barqdb/barq-server/internal/dataplane"
	"github.com/barqdb/barq-server/internal/syncrules"
	"github.com/barqdb/barq-server/internal/transforms"
	"github.com/barqdb/barq-server/internal/webhooks"
)

func TestDataAPITenantAuthBeforeDataPlane(t *testing.T) {
	server := testServer(t)
	forbidden := request(t, server.Client(), http.MethodGet, server.URL+"/v1/tenants/b/databases/main/objects/Task/one", "key-a", nil, nil)
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-tenant request was not forbidden before reaching the data plane: %d", forbidden.StatusCode)
	}
}

func TestSyncRuleAPIRequiresAdminAndKeepsRevisionHistory(t *testing.T) {
	var current dataplane.FLXRuleSet
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/v1/schema/read":
			_ = json.NewEncoder(w).Encode(dataplane.Schema{Version: 1, Objects: []dataplane.SchemaObject{{Name: "Order"}}})
		case "/internal/v1/flx/rules/read":
			_ = json.NewEncoder(w).Encode(current)
		case "/internal/v1/flx/rules/plan", "/internal/v1/flx/rules/apply":
			var input dataplane.FLXRulesChangeRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			candidate := dataplane.FLXRuleSet{Revision: current.Revision + 1, Hash: "hash-one", Source: "database", Rules: input.Rules}
			if r.URL.Path == "/internal/v1/flx/rules/apply" {
				current = candidate
			}
			_ = json.NewEncoder(w).Encode(dataplane.FLXRulesResult{FLXRuleSet: candidate, CurrentRevision: input.ExpectedRevision, TargetRevision: candidate.Revision, Applied: r.URL.Path == "/internal/v1/flx/rules/apply"})
		case "/internal/v1/flx/rules/test":
			_ = json.NewEncoder(w).Encode(dataplane.FLXRulesTestResult{ObjectType: "Order", Found: true, Configured: true, CanRead: true, CanWrite: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(core.Close)
	data, err := dataplane.NewHTTPDataPlane(core.URL, "", core.Client())
	if err != nil {
		t.Fatal(err)
	}
	store, err := control.OpenBarqStore(t.TempDir() + "/control.barq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keys := auth.NewManager(store)
	if err := keys.Bootstrap(t.Context(), auth.BootstrapOptions{APIKeys: "sync-key:a:main:sync:admin,read-key:a:main:read"}); err != nil {
		t.Fatal(err)
	}
	hooks := webhooks.NewService(store, transforms.NewQuickJS(), true)
	server := httptest.NewServer(api.New(data, hooks, keys, syncrules.New(data, store)).Handler())
	t.Cleanup(server.Close)
	base := server.URL + "/v1/tenants/a/databases/main"

	forbidden := request(t, server.Client(), http.MethodGet, base+"/sync-rules", "read-key", nil, nil)
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("read key status = %d", forbidden.StatusCode)
	}
	_ = forbidden.Body.Close()
	schema := request(t, server.Client(), http.MethodGet, base+"/schema", "sync-key", nil, nil)
	if schema.StatusCode != http.StatusOK {
		t.Fatalf("schema status = %d: %s", schema.StatusCode, readBody(schema))
	}
	_ = schema.Body.Close()
	rules := []map[string]any{{"object_type": "Order", "read": "owner_id == $user.id", "write": "owner_id == $user.id"}}
	planned := request(t, server.Client(), http.MethodPost, base+"/sync-rules:plan", "sync-key", map[string]any{"expected_revision": 0, "rules": rules}, nil)
	if planned.StatusCode != http.StatusOK {
		t.Fatalf("plan status = %d: %s", planned.StatusCode, readBody(planned))
	}
	_ = planned.Body.Close()
	applied := request(t, server.Client(), http.MethodPut, base+"/sync-rules", "sync-key", map[string]any{"expected_revision": 0, "rules": rules}, map[string]string{"Idempotency-Key": "first-rules"})
	if applied.StatusCode != http.StatusOK || applied.Header.Get("X-Barq-Rule-Revision") != "1" {
		t.Fatalf("apply status = %d: %s", applied.StatusCode, readBody(applied))
	}
	_ = applied.Body.Close()
	history := request(t, server.Client(), http.MethodGet, base+"/sync-rules/revisions", "sync-key", nil, nil)
	body := readBody(history)
	if history.StatusCode != http.StatusOK || !strings.Contains(body, `"revision":1`) || !strings.Contains(body, `"request_id":"first-rules"`) {
		t.Fatalf("history status = %d: %s", history.StatusCode, body)
	}
	tested := request(t, server.Client(), http.MethodPost, base+"/sync-rules:test", "sync-key", map[string]any{"user_id": "user-1", "object_type": "Order", "primary_key": "order-1", "rules": rules}, nil)
	if tested.StatusCode != http.StatusOK || !strings.Contains(readBody(tested), `"can_read":true`) {
		t.Fatalf("test status = %d", tested.StatusCode)
	}
	restored := request(t, server.Client(), http.MethodPost, base+"/sync-rules/revisions/1:restore", "sync-key", map[string]any{"expected_revision": 1}, map[string]string{"Idempotency-Key": "restore-one"})
	if restored.StatusCode != http.StatusOK || restored.Header.Get("X-Barq-Rule-Revision") != "2" {
		t.Fatalf("restore status = %d: %s", restored.StatusCode, readBody(restored))
	}
	_ = restored.Body.Close()
}

func TestWebhookRegistrationAPI(t *testing.T) {
	server := testServer(t)
	response := request(t, server.Client(), http.MethodPost, server.URL+"/v1/webhooks", "key-a", map[string]any{
		"name": "tasks", "scope": map[string]any{"tenant": "a", "database": "main"},
		"url": "http://127.0.0.1/hook", "events": []string{"Task.created"},
		"transform": map[string]any{"language": "javascript", "source": "export function transform(ctx) { return {id: ctx.after.id}; }"},
	}, nil)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("register status %d: %s", response.StatusCode, readBody(response))
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("one-time webhook secret response is cacheable")
	}
	var registered webhooks.Registered
	if err := json.NewDecoder(response.Body).Decode(&registered); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if registered.Secret == "" || registered.Webhook.ID == "" || registered.Webhook.ActiveRevision != 1 {
		t.Fatalf("incomplete registration: %+v", registered)
	}
	list := request(t, server.Client(), http.MethodGet, server.URL+"/v1/webhooks", "key-a", nil, nil)
	if list.StatusCode != http.StatusOK || !bytes.Contains([]byte(readBody(list)), []byte(registered.Webhook.ID)) {
		t.Fatalf("registered hook missing from list")
	}
}

func TestEmbeddedControlAndSwagger(t *testing.T) {
	server := testServer(t)
	for _, path := range []string{"/control/", "/control/app.css", "/control/app.js", "/docs/", "/docs/openapi.yaml"} {
		response, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(response)
		if response.StatusCode != http.StatusOK || body == "" {
			t.Fatalf("embedded route %s: status=%d body=%q", path, response.StatusCode, body)
		}
		if path == "/control/" && !strings.Contains(body, "Service keys") {
			t.Fatal("embedded control UI is missing access management")
		}
		if path == "/docs/openapi.yaml" && !strings.Contains(body, "/v1/admin/api-keys") {
			t.Fatal("public OpenAPI is missing access management")
		}
	}
}

func TestOperationalHealthRequiresKeyAndReturnsScopedCounts(t *testing.T) {
	server := testServer(t)
	unauthorized, err := server.Client().Get(server.URL + "/v1/operations/health")
	if err != nil {
		t.Fatal(err)
	}
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.StatusCode)
	}
	_ = unauthorized.Body.Close()
	forbidden := request(t, server.Client(), http.MethodGet, server.URL+"/v1/operations/health", "key-read", nil, nil)
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("missing operations permission status = %d", forbidden.StatusCode)
	}
	_ = forbidden.Body.Close()
	response := request(t, server.Client(), http.MethodGet, server.URL+"/v1/operations/health", "key-a", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("operations health status = %d: %s", response.StatusCode, readBody(response))
	}
	var health webhooks.OperationalHealth
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if health.Status != "ok" || health.Pending != 0 || health.DeadDelivery != 0 {
		t.Fatalf("unexpected health: %+v", health)
	}
}

func TestTenantAndAPIKeyAdmin(t *testing.T) {
	server := testServer(t)
	forbiddenTenant := request(t, server.Client(), http.MethodPost, server.URL+"/v1/admin/tenants", "key-a", map[string]any{
		"id": "blocked", "name": "Blocked", "databases": []string{"main"},
	}, nil)
	if forbiddenTenant.StatusCode != http.StatusForbidden {
		t.Fatalf("scoped key created a tenant: %d", forbiddenTenant.StatusCode)
	}
	_ = forbiddenTenant.Body.Close()
	createdTenant := request(t, server.Client(), http.MethodPost, server.URL+"/v1/admin/tenants", "root-key", map[string]any{
		"id": "b", "name": "Tenant B", "databases": []string{"main", "audit"},
	}, nil)
	if createdTenant.StatusCode != http.StatusCreated {
		t.Fatalf("create tenant status %d: %s", createdTenant.StatusCode, readBody(createdTenant))
	}
	_ = createdTenant.Body.Close()
	forbiddenKey := request(t, server.Client(), http.MethodPost, server.URL+"/v1/admin/api-keys", "key-a", map[string]any{
		"label": "Cross tenant", "tenant": "b", "database": "main", "actions": []string{"read"},
	}, nil)
	if forbiddenKey.StatusCode != http.StatusForbidden {
		t.Fatalf("scoped key created a cross-tenant key: %d", forbiddenKey.StatusCode)
	}
	_ = forbiddenKey.Body.Close()

	createdKey := request(t, server.Client(), http.MethodPost, server.URL+"/v1/admin/api-keys", "root-key", map[string]any{
		"label": "Tenant B app", "tenant": "b", "database": "main", "actions": []string{"read", "write"},
	}, nil)
	if createdKey.StatusCode != http.StatusCreated {
		t.Fatalf("create key status %d: %s", createdKey.StatusCode, readBody(createdKey))
	}
	if createdKey.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("one-time API key response is cacheable")
	}
	var created auth.CreatedServiceKey
	if err := json.NewDecoder(createdKey.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	_ = createdKey.Body.Close()
	if created.Secret == "" || created.Key.ID == "" {
		t.Fatalf("incomplete one-time key response: %+v", created)
	}

	list := request(t, server.Client(), http.MethodGet, server.URL+"/v1/admin/api-keys", "root-key", nil, nil)
	listBody := readBody(list)
	if list.StatusCode != http.StatusOK || !strings.Contains(listBody, created.Key.ID) || strings.Contains(listBody, "digest") || strings.Contains(listBody, created.Secret) {
		t.Fatalf("unsafe API key list: status=%d body=%s", list.StatusCode, listBody)
	}

	oldSecret := created.Secret
	rotatedResponse := request(t, server.Client(), http.MethodPost, server.URL+"/v1/admin/api-keys/"+created.Key.ID+":rotate", "root-key", map[string]any{}, nil)
	if rotatedResponse.StatusCode != http.StatusCreated {
		t.Fatalf("rotate status %d: %s", rotatedResponse.StatusCode, readBody(rotatedResponse))
	}
	var rotated auth.CreatedServiceKey
	if err := json.NewDecoder(rotatedResponse.Body).Decode(&rotated); err != nil {
		t.Fatal(err)
	}
	_ = rotatedResponse.Body.Close()
	if rotated.Secret == "" || rotated.Secret == oldSecret {
		t.Fatal("rotation did not return a new one-time secret")
	}

	oldRequest := request(t, server.Client(), http.MethodGet, server.URL+"/v1/tenants/b/databases/main/objects/Task/one", oldSecret, nil, nil)
	if oldRequest.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rotated secret still works: %d", oldRequest.StatusCode)
	}
	_ = oldRequest.Body.Close()
	newRequest := request(t, server.Client(), http.MethodGet, server.URL+"/v1/tenants/b/databases/main/objects/Task/one", rotated.Secret, nil, nil)
	if newRequest.StatusCode == http.StatusUnauthorized || newRequest.StatusCode == http.StatusForbidden {
		t.Fatalf("replacement secret was rejected before the data plane: %d", newRequest.StatusCode)
	}
	_ = newRequest.Body.Close()
}

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	data, err := dataplane.NewHTTPDataPlane("http://127.0.0.1:1", "test-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := control.OpenBarqStore(t.TempDir() + "/control.barq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runtime := transforms.NewQuickJS()
	hooks := webhooks.NewService(store, runtime, true)
	keys := auth.NewManager(store)
	if err := keys.Bootstrap(t.Context(), auth.BootstrapOptions{
		APIKeys: "root-key:*:*:*,key-a:a:main:*,key-read:a:main:read",
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.New(data, hooks, keys, syncrules.New(data, store)).Handler())
	t.Cleanup(server.Close)
	return server
}

func request(t *testing.T, client *http.Client, method, url, key string, body any, headers map[string]string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readBody(response *http.Response) string {
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	return string(body)
}
