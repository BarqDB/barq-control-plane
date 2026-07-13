package dataplane_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/barqdb/barq-server/internal/dataplane"
)

func TestHTTPDataPlaneUsesVersionedRouteAndSecret(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/write" || r.Header.Get("Authorization") != "Bearer private-secret" {
			http.Error(w, "wrong request", http.StatusBadRequest)
			return
		}
		var request dataplane.WriteRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Scope.Tenant != "a" {
			http.Error(w, "bad JSON", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(dataplane.WriteResult{Deleted: true, Cursor: 7})
	}))
	defer server.Close()
	data, err := dataplane.NewHTTPDataPlane(server.URL, "private-secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := data.WriteObject(context.Background(), dataplane.WriteRequest{
		Scope: dataplane.Scope{Tenant: "a", Database: "main"}, Operation: dataplane.WriteDelete,
		Type: "Task", PrimaryKey: "one", IfMatch: `"etag"`,
	})
	if err != nil || result.Cursor != 7 {
		t.Fatalf("unexpected result: %+v %v", result, err)
	}
}

func TestHTTPDataPlaneDecodesTypedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(dataplane.Error{Code: dataplane.CodeConflict, Message: "stale"})
	}))
	defer server.Close()
	data, _ := dataplane.NewHTTPDataPlane(server.URL, "", server.Client())
	_, err := data.ReadObject(context.Background(), dataplane.ReadRequest{})
	if !dataplane.IsCode(err, dataplane.CodeConflict) {
		t.Fatalf("expected typed conflict, got %v", err)
	}
}
