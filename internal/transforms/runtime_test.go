package transforms_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/barqdb/barq-server/internal/transforms"
)

func TestQuickJSTransform(t *testing.T) {
	runtime := transforms.NewQuickJS()
	source := `
export function filter(ctx) { return ctx.after.status === "paid"; }
export function transform(ctx) {
  return {order_id: ctx.after.id, email: ctx.related.customer.email};
}`
	input := map[string]any{
		"after":   map[string]any{"id": "o-1", "status": "paid"},
		"related": map[string]any{"customer": map[string]any{"email": "a@example.com"}},
	}
	result, err := runtime.Execute(context.Background(), source, input, transforms.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Matched || string(result.Payload) != `{"order_id":"o-1","email":"a@example.com"}` {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestQuickJSFilterCanSkip(t *testing.T) {
	runtime := transforms.NewQuickJS()
	source := `function filter() { return false; } function transform() { return {bad: true}; }`
	result, err := runtime.Execute(context.Background(), source, map[string]any{}, transforms.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if result.Matched || len(result.Payload) != 0 {
		t.Fatalf("expected skipped result: %+v", result)
	}
}

func TestQuickJSRejectsCapabilities(t *testing.T) {
	runtime := transforms.NewQuickJS()
	for _, source := range []string{
		`import x from "x"; export function transform() { return x; }`,
		`async function transform() { return {}; }`,
		`function transform() { return require("fs"); }`,
	} {
		if err := runtime.Validate(context.Background(), source, transforms.DefaultLimits()); err == nil {
			t.Fatalf("expected source to be rejected: %s", source)
		}
	}
}

func TestQuickJSTimeout(t *testing.T) {
	runtime := transforms.NewQuickJS()
	limits := transforms.DefaultLimits()
	limits.Timeout = 20 * time.Millisecond
	_, err := runtime.Execute(context.Background(), `function transform() { while (true) {} }`, map[string]any{}, limits)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestQuickJSHasNoClockOrRandomCapability(t *testing.T) {
	runtime := transforms.NewQuickJS()
	for _, source := range []string{
		`function transform() { return {now: Date.now()}; }`,
		`function transform() { return {random: Math.random()}; }`,
	} {
		if _, err := runtime.Execute(context.Background(), source, map[string]any{}, transforms.DefaultLimits()); err == nil {
			t.Fatalf("expected disabled capability to fail: %s", source)
		}
	}
}
