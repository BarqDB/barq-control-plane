package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/barqdb/barq-server/internal/api"
	"github.com/barqdb/barq-server/internal/auth"
	"github.com/barqdb/barq-server/internal/control"
	"github.com/barqdb/barq-server/internal/dataplane"
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
	}
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
	keys := auth.NewMemoryKeyStore()
	keys.Add("key-a", auth.ServiceKey{Tenant: "a", Database: "main", Actions: []string{"*"}})
	server := httptest.NewServer(api.New(data, hooks, keys).Handler())
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
