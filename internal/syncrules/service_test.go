package syncrules_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/barqdb/barq-server/internal/control"
	"github.com/barqdb/barq-server/internal/dataplane"
	"github.com/barqdb/barq-server/internal/syncrules"
)

type coreProtocol struct {
	mu                sync.Mutex
	current           dataplane.FLXRuleSet
	dropApplyResponse bool
}

func (c *coreProtocol) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer private" {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(dataplane.Error{Code: dataplane.CodeUnauthorized, Message: "bad secret"})
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.URL.Path {
	case "/internal/v1/schema/read":
		_ = json.NewEncoder(w).Encode(dataplane.Schema{Version: 4, Objects: []dataplane.SchemaObject{{Name: "Order"}}})
	case "/internal/v1/flx/rules/read":
		_ = json.NewEncoder(w).Encode(c.current)
	case "/internal/v1/flx/rules/plan", "/internal/v1/flx/rules/apply":
		var input dataplane.FLXRulesChangeRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rulesJSON, _ := json.Marshal(input.Rules)
		candidate := dataplane.FLXRuleSet{Revision: input.TargetRevision, Hash: fmt.Sprintf("%x", sha256.Sum256(rulesJSON)), Source: "database", Rules: input.Rules}
		if r.URL.Path == "/internal/v1/flx/rules/plan" {
			candidate.Revision = c.current.Revision + 1
			_ = json.NewEncoder(w).Encode(dataplane.FLXRulesResult{FLXRuleSet: candidate, CurrentRevision: c.current.Revision, TargetRevision: candidate.Revision})
			return
		}
		if input.TargetRevision == c.current.Revision && candidate.Hash == c.current.Hash {
			_ = json.NewEncoder(w).Encode(dataplane.FLXRulesResult{FLXRuleSet: c.current, Applied: true})
			return
		}
		if input.ExpectedRevision != c.current.Revision || input.TargetRevision != c.current.Revision+1 {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(dataplane.Error{Code: dataplane.CodeConflict, Message: "stale revision"})
			return
		}
		previous := c.current.Revision
		c.current = candidate
		if c.dropApplyResponse {
			c.dropApplyResponse = false
			_, _ = w.Write([]byte("{"))
			return
		}
		_ = json.NewEncoder(w).Encode(dataplane.FLXRulesResult{FLXRuleSet: candidate, CurrentRevision: previous, TargetRevision: candidate.Revision, Applied: true})
	case "/internal/v1/flx/rules/test":
		_ = json.NewEncoder(w).Encode(dataplane.FLXRulesTestResult{ObjectType: "Order", Found: true, Configured: true, CanRead: true, CanWrite: false})
	default:
		http.NotFound(w, r)
	}
}

func TestApplyStoresImmutableHistoryAndSupportsRetryAndRestore(t *testing.T) {
	core := &coreProtocol{current: dataplane.FLXRuleSet{Revision: 0, Hash: "bootstrap", Source: "bootstrap", Rules: []dataplane.FLXRule{}}}
	server := httptest.NewServer(core)
	t.Cleanup(server.Close)
	data, err := dataplane.NewHTTPDataPlane(server.URL, "private", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	store, err := control.OpenBarqStore(t.TempDir() + "/control.barq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := syncrules.New(data, store)
	scope := dataplane.Scope{Tenant: "a", Database: "main"}
	rules := []dataplane.FLXRule{{ObjectType: "Order", Read: "owner_id == $user.id", Write: "owner_id == $user.id"}}
	input := syncrules.ApplyInput{ExpectedRevision: 0, Rules: rules, RequestID: "apply-one", Actor: "key-1"}
	result, err := service.Apply(t.Context(), scope, input)
	if err != nil || result.Revision != 1 || !result.Applied {
		t.Fatalf("apply: %+v, %v", result, err)
	}
	if _, err := service.Apply(t.Context(), scope, input); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	stale := syncrules.ApplyInput{
		ExpectedRevision: 0,
		Rules:            []dataplane.FLXRule{{ObjectType: "Order", Read: "TRUEPREDICATE", Write: "TRUEPREDICATE"}},
		RequestID:        "stale-apply",
		Actor:            "key-2",
	}
	if _, err := service.Apply(t.Context(), scope, stale); !dataplane.IsCode(err, dataplane.CodeConflict) {
		t.Fatalf("stale apply should conflict: %v", err)
	}
	if err := service.Reconcile(t.Context()); err != nil {
		t.Fatalf("rejected apply poisoned recovery: %v", err)
	}
	history, err := service.History(t.Context(), scope)
	if err != nil || len(history) != 1 || history[0].Actor != "key-1" || history[0].Source != "apply" {
		t.Fatalf("history: %+v, %v", history, err)
	}
	restored, err := service.Restore(t.Context(), scope, 1, syncrules.ApplyInput{ExpectedRevision: 1, RequestID: "restore-one", Actor: "key-2"})
	if err != nil || restored.Revision != 2 {
		t.Fatalf("restore: %+v, %v", restored, err)
	}
	history, err = service.History(t.Context(), scope)
	if err != nil || len(history) != 2 || history[0].Revision != 2 || history[0].Source != "restore" ||
		history[0].RestoredFrom == nil || *history[0].RestoredFrom != 1 || history[1].Revision != 1 {
		t.Fatalf("restored history: %+v, %v", history, err)
	}
}

func TestCurrentRecoversInterruptedApplyFromCore(t *testing.T) {
	rules := []dataplane.FLXRule{{ObjectType: "Order", Read: "TRUEPREDICATE", Write: "FALSEPREDICATE"}}
	core := &coreProtocol{current: dataplane.FLXRuleSet{Revision: 7, Hash: "persisted-hash", Source: "database", Rules: rules}}
	server := httptest.NewServer(core)
	t.Cleanup(server.Close)
	data, _ := dataplane.NewHTTPDataPlane(server.URL, "private", server.Client())
	store, err := control.OpenBarqStore(t.TempDir() + "/control.barq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := syncrules.New(data, store)
	scope := dataplane.Scope{Tenant: "b", Database: "audit"}
	current, err := service.Current(t.Context(), scope, "reconcile-startup")
	if err != nil || current.Revision != 7 {
		t.Fatalf("current: %+v, %v", current, err)
	}
	history, err := service.History(t.Context(), scope)
	if err != nil || len(history) != 1 || history[0].Revision != 7 || history[0].Source != "recovered" {
		t.Fatalf("recovered history: %+v, %v", history, err)
	}
}

func TestReconcileFinishesApplyAfterCoreResponseWasLost(t *testing.T) {
	core := &coreProtocol{current: dataplane.FLXRuleSet{Revision: 0, Hash: "bootstrap", Rules: []dataplane.FLXRule{}}, dropApplyResponse: true}
	server := httptest.NewServer(core)
	t.Cleanup(server.Close)
	data, _ := dataplane.NewHTTPDataPlane(server.URL, "private", server.Client())
	store, err := control.OpenBarqStore(t.TempDir() + "/control.barq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := syncrules.New(data, store)
	scope := dataplane.Scope{Tenant: "a", Database: "main"}
	rules := []dataplane.FLXRule{{ObjectType: "Order", Read: "TRUEPREDICATE", Write: "FALSEPREDICATE"}}
	if _, err := service.Apply(t.Context(), scope, syncrules.ApplyInput{ExpectedRevision: 0, Rules: rules, RequestID: "lost-response", Actor: "key-1"}); err == nil {
		t.Fatal("apply should report the lost Core response")
	}
	if err := service.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	history, err := service.History(t.Context(), scope)
	if err != nil || len(history) != 1 || history[0].Revision != 1 || history[0].Source != "recovered" {
		t.Fatalf("history: %+v, %v", history, err)
	}
}

func TestReconcileDropsARecoveredApplySupersededByANewerRevision(t *testing.T) {
	core := &coreProtocol{current: dataplane.FLXRuleSet{Revision: 0, Hash: "bootstrap", Rules: []dataplane.FLXRule{}}, dropApplyResponse: true}
	server := httptest.NewServer(core)
	t.Cleanup(server.Close)
	data, _ := dataplane.NewHTTPDataPlane(server.URL, "private", server.Client())
	store, err := control.OpenBarqStore(t.TempDir() + "/control.barq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := syncrules.New(data, store)
	scope := dataplane.Scope{Tenant: "a", Database: "main"}
	first := []dataplane.FLXRule{{ObjectType: "Order", Read: "TRUEPREDICATE", Write: "FALSEPREDICATE"}}
	if _, err := service.Apply(t.Context(), scope, syncrules.ApplyInput{ExpectedRevision: 0, Rules: first, RequestID: "lost-first"}); err == nil {
		t.Fatal("first apply should lose its response")
	}
	second := []dataplane.FLXRule{{ObjectType: "Order", Read: "TRUEPREDICATE", Write: "TRUEPREDICATE"}}
	if _, err := service.Apply(t.Context(), scope, syncrules.ApplyInput{ExpectedRevision: 1, Rules: second, RequestID: "second"}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if err := service.Reconcile(t.Context()); err != nil {
		t.Fatalf("superseded recovery blocked startup: %v", err)
	}
}
