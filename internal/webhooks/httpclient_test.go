package webhooks_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/barqdb/barq-server/internal/webhooks"
)

func TestWebhookClientBlocksPrivateTargets(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer target.Close()
	client := webhooks.NewWebhookHTTPClient(false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, nil)
	if _, err := client.Do(request); err == nil {
		t.Fatal("private target should be blocked")
	}
}

func TestWebhookClientAllowsPrivateTargetsWhenEnabled(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	client := webhooks.NewWebhookHTTPClient(true)
	request, _ := http.NewRequest(http.MethodPost, target.URL, nil)
	response, err := client.Do(request)
	if err != nil || response.StatusCode != http.StatusNoContent {
		t.Fatalf("enabled private target failed: %v", err)
	}
	_ = response.Body.Close()
}
