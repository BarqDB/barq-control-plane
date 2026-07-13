package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/barqdb/barq-server/internal/dataplane"
)

type ServiceKey struct {
	ID        string    `json:"id"`
	Label     string    `json:"label,omitempty"`
	Digest    string    `json:"digest"`
	Tenant    string    `json:"tenant"`
	Database  string    `json:"database"`
	Actions   []string  `json:"actions"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type KeyStore interface {
	FindByDigest(context.Context, string) (ServiceKey, error)
}

type principalKey struct{}

func PrincipalFromContext(ctx context.Context) (ServiceKey, bool) {
	key, ok := ctx.Value(principalKey{}).(ServiceKey)
	return key, ok
}

func Middleware(store KeyStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") || len(header) <= len("Bearer ") {
			writeAuthError(w, http.StatusUnauthorized, "missing bearer API key")
			return
		}
		raw := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		key, err := store.FindByDigest(r.Context(), Digest(raw))
		if err != nil || !key.Enabled || subtle.ConstantTimeCompare([]byte(key.Digest), []byte(Digest(raw))) != 1 {
			writeAuthError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, key)))
	})
}

func Authorize(ctx context.Context, scope dataplane.Scope, action string) error {
	key, ok := PrincipalFromContext(ctx)
	if !ok {
		return &dataplane.Error{Code: dataplane.CodeUnauthorized, Message: "authentication required"}
	}
	if !CanAccessScope(ctx, scope) {
		if key.Tenant != "*" && key.Tenant != scope.Tenant {
			return &dataplane.Error{Code: dataplane.CodeForbidden, Message: "API key cannot access this tenant"}
		}
		return &dataplane.Error{Code: dataplane.CodeForbidden, Message: "API key cannot access this database"}
	}
	return AuthorizeAction(ctx, action)
}

func CanAccessScope(ctx context.Context, scope dataplane.Scope) bool {
	key, ok := PrincipalFromContext(ctx)
	if !ok {
		return false
	}
	return (key.Tenant == "*" || key.Tenant == scope.Tenant) &&
		(key.Database == "*" || key.Database == scope.Database)
}

func AuthorizeAction(ctx context.Context, action string) error {
	key, ok := PrincipalFromContext(ctx)
	if !ok {
		return &dataplane.Error{Code: dataplane.CodeUnauthorized, Message: "authentication required"}
	}
	for _, allowed := range key.Actions {
		if allowed == "*" || allowed == action {
			return nil
		}
	}
	return &dataplane.Error{Code: dataplane.CodeForbidden, Message: "API key is missing action " + action}
}

func AuthorizeGlobal(ctx context.Context, action string) error {
	if err := AuthorizeAction(ctx, action); err != nil {
		return err
	}
	key, ok := PrincipalFromContext(ctx)
	if !ok || key.Tenant != "*" || key.Database != "*" {
		return &dataplane.Error{Code: dataplane.CodeForbidden, Message: "a global admin API key is required"}
	}
	return nil
}

func Digest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func writeAuthError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"code":"unauthorized","message":"` + message + `"}`))
}
