package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/barqdb/barq-server/contracts"
	"github.com/barqdb/barq-server/internal/auth"
	"github.com/barqdb/barq-server/internal/console"
	"github.com/barqdb/barq-server/internal/dataplane"
	"github.com/barqdb/barq-server/internal/webhooks"
	"github.com/swaggest/swgui/v5"
)

type Server struct {
	data  dataplane.DataPlane
	hooks *webhooks.Service
	keys  *auth.Manager
}

func New(data dataplane.DataPlane, hooks *webhooks.Service, keys *auth.Manager) *Server {
	return &Server{data: data, hooks: hooks, keys: keys}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/control/", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /control", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/control/", http.StatusPermanentRedirect)
	})
	mux.Handle("/control/", http.StripPrefix("/control/", console.Handler()))
	mux.HandleFunc("GET /docs/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(contracts.PublicOpenAPI())
	})
	mux.Handle("/docs/", v5.New("Barq Server API", "/docs/openapi.yaml", "/docs/"))
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /health/ready", s.ready)
	protected := http.NewServeMux()
	protected.HandleFunc("/v1/tenants/", s.dataRoutes)
	protected.HandleFunc("/v1/webhooks", s.webhookCollection)
	protected.HandleFunc("/v1/webhooks/", s.webhookMember)
	protected.HandleFunc("/v1/admin/tenants", s.tenantCollection)
	protected.HandleFunc("/v1/admin/tenants/", s.tenantMember)
	protected.HandleFunc("/v1/admin/api-keys", s.apiKeyCollection)
	protected.HandleFunc("/v1/admin/api-keys/", s.apiKeyMember)
	protected.HandleFunc("GET /v1/operations/health", s.operationsHealth)
	mux.Handle("/v1/", auth.Middleware(s.keys, protected))
	return requestIDMiddleware(mux)
}

func (s *Server) tenantCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if err := auth.AuthorizeAction(r.Context(), "tenants:admin"); err != nil {
			writeError(w, err)
			return
		}
		tenants, err := s.keys.ListTenants(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		principal, _ := auth.PrincipalFromContext(r.Context())
		if principal.Tenant != "*" {
			visible := tenants[:0]
			for _, tenant := range tenants {
				if tenant.ID == principal.Tenant {
					visible = append(visible, tenant)
				}
			}
			tenants = visible
		}
		writeJSON(w, http.StatusOK, map[string]any{"tenants": tenants})
	case http.MethodPost:
		if err := auth.AuthorizeGlobal(r.Context(), "tenants:admin"); err != nil {
			writeError(w, err)
			return
		}
		var input auth.CreateTenantInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, err)
			return
		}
		tenant, err := s.keys.CreateTenant(r.Context(), input)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, tenant)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"code": "method_not_allowed", "message": "method not allowed"})
	}
}

func (s *Server) tenantMember(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/admin/tenants/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, &dataplane.Error{Code: dataplane.CodeNotFound, Message: "route not found"})
		return
	}
	tenant, err := s.keys.GetTenant(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if err := auth.Authorize(r.Context(), dataplane.Scope{Tenant: tenant.ID, Database: "*"}, "tenants:admin"); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, tenant)
	case http.MethodPatch:
		if err := auth.AuthorizeGlobal(r.Context(), "tenants:admin"); err != nil {
			writeError(w, err)
			return
		}
		var input auth.UpdateTenantInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, err)
			return
		}
		updated, err := s.keys.UpdateTenant(r.Context(), id, input)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	case http.MethodDelete:
		if err := auth.AuthorizeGlobal(r.Context(), "tenants:admin"); err != nil {
			writeError(w, err)
			return
		}
		if err := s.keys.DisableTenant(r.Context(), id); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"code": "method_not_allowed", "message": "method not allowed"})
	}
}

func (s *Server) apiKeyCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if err := auth.AuthorizeAction(r.Context(), "keys:admin"); err != nil {
			writeError(w, err)
			return
		}
		keys, err := s.keys.ListKeys(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		views := make([]auth.ServiceKeyView, 0, len(keys))
		for _, key := range keys {
			if canManageKey(r.Context(), key) {
				views = append(views, auth.PublicKey(key))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"api_keys": views})
	case http.MethodPost:
		var input auth.CreateServiceKeyInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, err)
			return
		}
		if err := authorizeKeyScope(r.Context(), input.Tenant, input.Database); err != nil {
			writeError(w, err)
			return
		}
		created, err := s.keys.CreateKey(r.Context(), input)
		if err != nil {
			writeError(w, err)
			return
		}
		writeOneTimeSecret(w, http.StatusCreated, created)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"code": "method_not_allowed", "message": "method not allowed"})
	}
}

func (s *Server) apiKeyMember(w http.ResponseWriter, r *http.Request) {
	member := strings.TrimPrefix(r.URL.Path, "/v1/admin/api-keys/")
	parts := strings.SplitN(member, ":", 2)
	id, action := parts[0], ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if id == "" || strings.Contains(id, "/") {
		writeError(w, &dataplane.Error{Code: dataplane.CodeNotFound, Message: "route not found"})
		return
	}
	key, err := s.keys.GetKey(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if !canManageKey(r.Context(), key) {
		writeError(w, &dataplane.Error{Code: dataplane.CodeForbidden, Message: "API key cannot manage this key scope"})
		return
	}
	if action != "" {
		if action != "rotate" || r.Method != http.MethodPost {
			writeError(w, &dataplane.Error{Code: dataplane.CodeNotFound, Message: "route not found"})
			return
		}
		created, err := s.keys.RotateKey(r.Context(), id)
		if err != nil {
			writeError(w, err)
			return
		}
		writeOneTimeSecret(w, http.StatusCreated, created)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, auth.PublicKey(key))
	case http.MethodPatch:
		var input auth.UpdateServiceKeyInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, err)
			return
		}
		updated, err := s.keys.UpdateKey(r.Context(), id, input)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	case http.MethodDelete:
		if err := s.keys.RevokeKey(r.Context(), id); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"code": "method_not_allowed", "message": "method not allowed"})
	}
}

func authorizeKeyScope(ctx context.Context, tenant, database string) error {
	if tenant == "*" || database == "*" {
		principal, ok := auth.PrincipalFromContext(ctx)
		if !ok || (tenant == "*" && (principal.Tenant != "*" || principal.Database != "*")) || (tenant != "*" && principal.Tenant != "*" && (principal.Tenant != tenant || principal.Database != "*")) {
			return &dataplane.Error{Code: dataplane.CodeForbidden, Message: "API key cannot create this key scope"}
		}
	}
	return auth.Authorize(ctx, dataplane.Scope{Tenant: tenant, Database: database}, "keys:admin")
}

func canManageKey(ctx context.Context, key auth.ServiceKey) bool {
	return authorizeKeyScope(ctx, key.Tenant, key.Database) == nil
}

func (s *Server) operationsHealth(w http.ResponseWriter, r *http.Request) {
	if err := auth.AuthorizeAction(r.Context(), "operations:read"); err != nil {
		writeError(w, err)
		return
	}
	health, err := s.hooks.OperationalHealth(r.Context(), func(scope dataplane.Scope) bool {
		return auth.CanAccessScope(r.Context(), scope)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, health)
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	health, err := s.data.Health(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, health)
}

func (s *Server) dataRoutes(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/v1/"))
	if len(parts) < 4 || parts[0] != "tenants" || parts[2] != "databases" {
		writeError(w, &dataplane.Error{Code: dataplane.CodeNotFound, Message: "route not found"})
		return
	}
	tenant, database := parts[1], parts[3]
	if strings.HasSuffix(database, ":batch") {
		database = strings.TrimSuffix(database, ":batch")
		if r.Method == http.MethodPost && len(parts) == 4 {
			s.batch(w, r, dataplane.Scope{Tenant: tenant, Database: database})
			return
		}
	}
	scope := dataplane.Scope{Tenant: tenant, Database: database}
	if len(parts) == 5 && strings.HasPrefix(parts[4], "schema:") && r.Method == http.MethodPost {
		s.schema(w, r, scope, strings.TrimPrefix(parts[4], "schema:"))
		return
	}
	if len(parts) < 6 || parts[4] != "objects" {
		writeError(w, &dataplane.Error{Code: dataplane.CodeNotFound, Message: "route not found"})
		return
	}
	objectType := parts[5]
	if strings.HasSuffix(objectType, ":query") && len(parts) == 6 && r.Method == http.MethodPost {
		s.query(w, r, scope, strings.TrimSuffix(objectType, ":query"))
		return
	}
	if len(parts) == 6 && r.Method == http.MethodPost {
		s.create(w, r, scope, objectType)
		return
	}
	if len(parts) == 7 {
		s.object(w, r, scope, objectType, parts[6])
		return
	}
	writeError(w, &dataplane.Error{Code: dataplane.CodeNotFound, Message: "route not found"})
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, scope dataplane.Scope, objectType string) {
	if err := auth.Authorize(r.Context(), scope, "write"); err != nil {
		writeError(w, err)
		return
	}
	var body struct {
		PrimaryKey any            `json:"primary_key"`
		Data       map[string]any `json:"data"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, err)
		return
	}
	result, err := s.data.WriteObject(r.Context(), dataplane.WriteRequest{
		RequestID: requestID(r.Context()), IdempotencyKey: r.Header.Get("Idempotency-Key"), Scope: scope,
		Operation: dataplane.WriteCreate, Type: objectType, PrimaryKey: body.PrimaryKey, Data: body.Data,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if result.Object != nil {
		w.Header().Set("ETag", result.Object.ETag)
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) object(w http.ResponseWriter, r *http.Request, scope dataplane.Scope, objectType, primaryKey string) {
	switch r.Method {
	case http.MethodGet:
		if err := auth.Authorize(r.Context(), scope, "read"); err != nil {
			writeError(w, err)
			return
		}
		object, err := s.data.ReadObject(r.Context(), dataplane.ReadRequest{RequestID: requestID(r.Context()), Scope: scope, Type: objectType, PrimaryKey: primaryKey})
		if err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("ETag", object.ETag)
		writeJSON(w, http.StatusOK, object)
	case http.MethodPatch:
		if err := auth.Authorize(r.Context(), scope, "write"); err != nil {
			writeError(w, err)
			return
		}
		var data map[string]any
		if err := decodeJSON(r, &data); err != nil {
			writeError(w, err)
			return
		}
		result, err := s.data.WriteObject(r.Context(), dataplane.WriteRequest{
			RequestID: requestID(r.Context()), IdempotencyKey: r.Header.Get("Idempotency-Key"), Scope: scope,
			Operation: dataplane.WritePatch, Type: objectType, PrimaryKey: primaryKey, Data: data, IfMatch: r.Header.Get("If-Match"),
		})
		if err != nil {
			writeError(w, err)
			return
		}
		if result.Object != nil {
			w.Header().Set("ETag", result.Object.ETag)
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodDelete:
		if err := auth.Authorize(r.Context(), scope, "write"); err != nil {
			writeError(w, err)
			return
		}
		result, err := s.data.WriteObject(r.Context(), dataplane.WriteRequest{
			RequestID: requestID(r.Context()), IdempotencyKey: r.Header.Get("Idempotency-Key"), Scope: scope,
			Operation: dataplane.WriteDelete, Type: objectType, PrimaryKey: primaryKey, IfMatch: r.Header.Get("If-Match"),
		})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"code": "method_not_allowed", "message": "method not allowed"})
	}
}

func (s *Server) query(w http.ResponseWriter, r *http.Request, scope dataplane.Scope, objectType string) {
	if err := auth.Authorize(r.Context(), scope, "read"); err != nil {
		writeError(w, err)
		return
	}
	var request dataplane.QueryRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	request.RequestID, request.Scope, request.Type = requestID(r.Context()), scope, objectType
	result, err := s.data.QueryObjects(r.Context(), request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) batch(w http.ResponseWriter, r *http.Request, scope dataplane.Scope) {
	if err := auth.Authorize(r.Context(), scope, "write"); err != nil {
		writeError(w, err)
		return
	}
	var request dataplane.BatchRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	request.RequestID, request.IdempotencyKey, request.Scope = requestID(r.Context()), r.Header.Get("Idempotency-Key"), scope
	result, err := s.data.ExecuteBatch(r.Context(), request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) schema(w http.ResponseWriter, r *http.Request, scope dataplane.Scope, action string) {
	if err := auth.Authorize(r.Context(), scope, "schema:admin"); err != nil {
		writeError(w, err)
		return
	}
	var request dataplane.SchemaRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	request.RequestID, request.Scope = requestID(r.Context()), scope
	var result dataplane.SchemaResult
	var err error
	switch action {
	case "plan":
		result, err = s.data.PlanSchema(r.Context(), request)
	case "apply":
		result, err = s.data.ApplySchema(r.Context(), request)
	default:
		err = &dataplane.Error{Code: dataplane.CodeNotFound, Message: "route not found"}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) webhookCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var input webhooks.Registration
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, err)
			return
		}
		if err := auth.Authorize(r.Context(), input.Scope, "webhooks:admin"); err != nil {
			writeError(w, err)
			return
		}
		registered, err := s.hooks.Register(r.Context(), input)
		if err != nil {
			writeError(w, err)
			return
		}
		writeOneTimeSecret(w, http.StatusCreated, registered)
	case http.MethodGet:
		hooks, err := s.hooks.List(r.Context(), nil)
		if err != nil {
			writeError(w, err)
			return
		}
		visible := hooks[:0]
		for _, hook := range hooks {
			if auth.Authorize(r.Context(), hook.Scope, "webhooks:admin") == nil {
				visible = append(visible, hook)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"webhooks": visible})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"code": "method_not_allowed", "message": "method not allowed"})
	}
}

func (s *Server) webhookMember(w http.ResponseWriter, r *http.Request) {
	member := strings.TrimPrefix(r.URL.Path, "/v1/webhooks/")
	parts := strings.SplitN(member, ":", 2)
	id, action := parts[0], ""
	if len(parts) == 2 {
		action = parts[1]
	}
	hook, err := s.hooks.Get(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := auth.Authorize(r.Context(), hook.Scope, "webhooks:admin"); err != nil {
		writeError(w, err)
		return
	}
	if action != "" && r.Method == http.MethodPost {
		s.webhookAction(w, r, hook, action)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, hook)
	case http.MethodPatch:
		var input webhooks.Registration
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, err)
			return
		}
		if err := auth.Authorize(r.Context(), input.Scope, "webhooks:admin"); err != nil {
			writeError(w, err)
			return
		}
		updated, err := s.hooks.Update(r.Context(), id, input)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	case http.MethodDelete:
		if err := s.hooks.Disable(r.Context(), id); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"code": "method_not_allowed", "message": "method not allowed"})
	}
}

func (s *Server) webhookAction(w http.ResponseWriter, r *http.Request, hook webhooks.Webhook, action string) {
	switch action {
	case "test":
		var event dataplane.EventContext
		if err := decodeJSON(r, &event); err != nil {
			writeError(w, err)
			return
		}
		result, err := s.hooks.Test(r.Context(), hook.ID, event)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case "rotate-secret":
		secret, err := s.hooks.RotateSecret(r.Context(), hook.ID)
		if err != nil {
			writeError(w, err)
			return
		}
		writeOneTimeSecret(w, http.StatusOK, map[string]string{"secret": secret})
	case "replay":
		count, err := s.hooks.Replay(r.Context(), hook.ID)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"replayed": count})
	default:
		writeError(w, &dataplane.Error{Code: dataplane.CodeNotFound, Message: "route not found"})
	}
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &dataplane.Error{Code: dataplane.CodeInvalid, Message: "invalid JSON: " + err.Error()}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &dataplane.Error{Code: dataplane.CodeInvalid, Message: "request body must contain one JSON value"}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeOneTimeSecret(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, status, value)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var apiError *dataplane.Error
	if !errors.As(err, &apiError) {
		apiError = &dataplane.Error{Code: dataplane.CodeInternal, Message: err.Error()}
	}
	switch apiError.Code {
	case dataplane.CodeInvalid:
		status = http.StatusBadRequest
	case dataplane.CodeUnauthorized:
		status = http.StatusUnauthorized
	case dataplane.CodeForbidden:
		status = http.StatusForbidden
	case dataplane.CodeNotFound:
		status = http.StatusNotFound
	case dataplane.CodeConflict:
		status = http.StatusConflict
	case dataplane.CodePrecondition:
		status = http.StatusPreconditionFailed
	case dataplane.CodeResourceExceeded:
		status = http.StatusTooManyRequests
	case dataplane.CodeUnavailable:
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, apiError)
}

func splitPath(path string) []string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		return nil
	}
	return parts
}

type requestIDKey struct{}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = strings.ReplaceAll(r.RemoteAddr, ":", "-") + "-request"
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func requestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}
