package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	"github.com/barqdb/barq-server/internal/syncrules"
	"github.com/barqdb/barq-server/internal/transforms"
	"github.com/barqdb/barq-server/internal/webhooks"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := migrationCommand(os.Args[2:]); err != nil {
			logger.Error("control database migration", "error", err)
			os.Exit(1)
		}
		logger.Info("control database migration passed")
		return
	}
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
	if err := control.EnsureSchema(context.Background(), store); err != nil {
		logger.Error("prepare control schema", "error", err)
		_ = store.Close()
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
	keys, err := makeKeys(store)
	if err != nil {
		logger.Error("configure API keys", "error", err)
		os.Exit(1)
	}
	dispatcher := webhooks.NewDispatcher(data, store, runtime, webhooks.NewWebhookHTTPClient(allowPrivate))
	ruleService := syncrules.New(data, store)
	reconcileCtx, cancelReconcile := context.WithTimeout(context.Background(), 10*time.Second)
	if err := ruleService.Reconcile(reconcileCtx); err != nil {
		cancelReconcile()
		logger.Error("reconcile sync rules", "error", err)
		os.Exit(1)
	}
	cancelReconcile()

	handler := api.New(data, hookService, keys, ruleService).Handler()
	address := os.Getenv("BARQ_LISTEN_ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if hasCapability(health, "changes") {
		go runDispatcher(ctx, logger, dispatcher, keys)
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

func migrationCommand(args []string) error {
	set := flag.NewFlagSet("migrate", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	check := set.Bool("check", false, "check whether the migration is supported")
	apply := set.Bool("apply", false, "apply the migration to the control database")
	from := set.Int("from", control.CurrentSchemaVersion, "current control schema version")
	to := set.Int("to", control.CurrentSchemaVersion, "target control schema version")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 || *check == *apply {
		return fmt.Errorf("use exactly one of --check or --apply")
	}
	if err := control.CheckMigration(*from, *to); err != nil {
		return err
	}
	if *check {
		return nil
	}
	store, err := control.OpenBarqStore(controlDatabasePath())
	if err != nil {
		return err
	}
	defer store.Close()
	return control.ApplyMigration(context.Background(), store, *from, *to)
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

func makeKeys(store control.Store) (*auth.Manager, error) {
	manager := auth.NewManager(store)
	err := manager.Bootstrap(context.Background(), auth.BootstrapOptions{
		APIKeys:         os.Getenv("BARQ_API_KEYS"),
		DevMode:         strings.EqualFold(os.Getenv("BARQ_DEV_MODE"), "true"),
		DefaultTenant:   os.Getenv("BARQ_BOOTSTRAP_TENANT"),
		DefaultDatabase: os.Getenv("BARQ_BOOTSTRAP_DATABASE"),
	})
	if err != nil {
		return nil, err
	}
	return manager, nil
}

func runDispatcher(ctx context.Context, logger *slog.Logger, dispatcher *webhooks.Dispatcher, manager *auth.Manager) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	scopeTicker := time.NewTicker(5 * time.Second)
	defer scopeTicker.Stop()
	var scopes []dataplane.Scope
	refreshScopes := func() {
		updated, err := manager.Scopes(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("tenant scope refresh failed", "error", err)
			}
			return
		}
		scopes = updated
	}
	refreshScopes()
	for {
		select {
		case <-ctx.Done():
			return
		case <-scopeTicker.C:
			refreshScopes()
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
