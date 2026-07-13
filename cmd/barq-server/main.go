package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/barqdb/barq-server/internal/api"
	"github.com/barqdb/barq-server/internal/auth"
	"github.com/barqdb/barq-server/internal/control"
	"github.com/barqdb/barq-server/internal/dataplane"
	"github.com/barqdb/barq-server/internal/transforms"
	"github.com/barqdb/barq-server/internal/webhooks"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	data, err := makeDataPlane()
	if err != nil {
		logger.Error("configure data plane", "error", err)
		os.Exit(1)
	}
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 10*time.Second)
	health, err := data.Health(startupCtx)
	cancelStartup()
	if err != nil {
		logger.Error("data plane is not ready", "error", err)
		os.Exit(1)
	}
	store, err := control.OpenBarqStore(controlDatabasePath())
	if err != nil {
		logger.Error("open control Barq", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Error("close control Barq", "error", err)
		}
	}()
	runtime := transforms.NewQuickJS()
	allowPrivate := strings.EqualFold(os.Getenv("BARQ_ALLOW_PRIVATE_WEBHOOKS"), "true")
	hookService := webhooks.NewService(store, runtime, allowPrivate)
	keys, scopes, err := makeKeys()
	if err != nil {
		logger.Error("configure API keys", "error", err)
		os.Exit(1)
	}
	dispatcher := webhooks.NewDispatcher(data, store, runtime, webhooks.NewWebhookHTTPClient(allowPrivate))

	handler := api.New(data, hookService, keys).Handler()
	address := os.Getenv("BARQ_LISTEN_ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if hasCapability(health, "changes") {
		go runDispatcher(ctx, logger, dispatcher, scopes)
	} else {
		logger.Info("webhook change polling disabled", "reason", "data plane does not advertise changes")
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	logger.Info("barq server listening", "address", address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func hasCapability(health dataplane.Health, capability string) bool {
	for _, item := range health.Capabilities {
		if item == capability {
			return true
		}
	}
	return false
}

func makeDataPlane() (dataplane.DataPlane, error) {
	if endpoint := os.Getenv("BARQ_DATA_PLANE_URL"); endpoint != "" {
		return dataplane.NewHTTPDataPlane(endpoint, os.Getenv("BARQ_DATA_PLANE_SECRET"), nil)
	}
	return nil, errors.New("BARQ_DATA_PLANE_URL is required")
}

func controlDatabasePath() string {
	if path := os.Getenv("BARQ_CONTROL_PATH"); path != "" {
		return path
	}
	dataDir := os.Getenv("BARQ_DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}
	return filepath.Join(dataDir, "_system", "control.barq")
}

func makeKeys() (*auth.MemoryKeyStore, []dataplane.Scope, error) {
	store := auth.NewMemoryKeyStore()
	config := os.Getenv("BARQ_API_KEYS")
	if config == "" {
		if !strings.EqualFold(os.Getenv("BARQ_DEV_MODE"), "true") {
			return nil, nil, errors.New("BARQ_API_KEYS is required (set BARQ_DEV_MODE=true only for local development)")
		}
		config = "dev-key:dev:default:*"
	}
	seenScopes := map[dataplane.Scope]bool{}
	var scopes []dataplane.Scope
	for _, entry := range strings.Split(config, ",") {
		parts := strings.SplitN(strings.TrimSpace(entry), ":", 4)
		if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
			return nil, nil, errors.New("invalid BARQ_API_KEYS entry; expected key:tenant:database:action|action")
		}
		key := auth.ServiceKey{Tenant: parts[1], Database: parts[2], Actions: strings.Split(parts[3], "|"), Enabled: true}
		store.Add(parts[0], key)
		scope := dataplane.Scope{Tenant: parts[1], Database: parts[2]}
		if scope.Tenant != "*" && scope.Database != "*" && !seenScopes[scope] {
			seenScopes[scope] = true
			scopes = append(scopes, scope)
		}
	}
	return store, scopes, nil
}

func runDispatcher(ctx context.Context, logger *slog.Logger, dispatcher *webhooks.Dispatcher, scopes []dataplane.Scope) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, scope := range scopes {
				if _, err := dispatcher.PollOnce(ctx, scope); err != nil && ctx.Err() == nil {
					logger.Warn("webhook change poll failed", "tenant", scope.Tenant, "database", scope.Database, "error", err)
				}
			}
			if _, err := dispatcher.DeliverDue(ctx, 100); err != nil && ctx.Err() == nil {
				logger.Warn("webhook delivery pass failed", "error", err)
			}
		}
	}
}
