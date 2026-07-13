package transforms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fastschema/qjs"
)

type Limits struct {
	SourceBytes  int           `json:"source_bytes"`
	MemoryBytes  int           `json:"memory_bytes"`
	StackBytes   int           `json:"stack_bytes"`
	Timeout      time.Duration `json:"timeout"`
	PayloadBytes int           `json:"payload_bytes"`
}

func DefaultLimits() Limits {
	return Limits{
		SourceBytes: 64 << 10, MemoryBytes: 16 << 20, StackBytes: 512 << 10,
		Timeout: 50 * time.Millisecond, PayloadBytes: 256 << 10,
	}
}

type Result struct {
	Matched bool            `json:"matched"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Runtime interface {
	Validate(context.Context, string, Limits) error
	Execute(context.Context, string, any, Limits) (Result, error)
}

type QuickJS struct{}

func NewQuickJS() *QuickJS { return &QuickJS{} }

var (
	exportFunction = regexp.MustCompile(`(?m)\bexport\s+function\s+(filter|transform)\s*\(`)
	forbidden      = []*regexp.Regexp{
		regexp.MustCompile(`(?m)\bimport\s*(?:\(|[\s{*])`),
		regexp.MustCompile(`(?m)\brequire\s*\(`),
		regexp.MustCompile(`(?m)\basync\s+function\b`),
		regexp.MustCompile(`(?m)\b(?:filter|transform)\s*=\s*async\b`),
	}
)

const lockedPrelude = `
"use strict";
globalThis.fetch = undefined;
globalThis.WebSocket = undefined;
globalThis.XMLHttpRequest = undefined;
globalThis.setTimeout = undefined;
globalThis.setInterval = undefined;
globalThis.clearTimeout = undefined;
globalThis.clearInterval = undefined;
globalThis.os = undefined;
globalThis.std = undefined;
globalThis.scriptArgs = undefined;
globalThis.Date = undefined;
Math.random = function () { throw new Error("random values are disabled"); };
globalThis.eval = undefined;
globalThis.Function = undefined;
`

func (q *QuickJS) Validate(ctx context.Context, source string, limits Limits) error {
	if err := validateSource(source, limits); err != nil {
		return err
	}
	_, err := q.run(ctx, source, map[string]any{
		"event": map[string]any{}, "before": nil, "after": map[string]any{}, "related": map[string]any{},
	}, limits, true)
	return err
}

func (q *QuickJS) Execute(ctx context.Context, source string, input any, limits Limits) (Result, error) {
	if err := validateSource(source, limits); err != nil {
		return Result{}, err
	}
	return q.run(ctx, source, input, limits, false)
}

func (q *QuickJS) run(parent context.Context, source string, input any, limits Limits, validateOnly bool) (result Result, err error) {
	if limits.Timeout <= 0 {
		limits.Timeout = DefaultLimits().Timeout
	}
	// Runtime startup includes Wasm compilation/instantiation and is cached but
	// can exceed a transform's tiny execution budget on the first call. Start
	// the hard timer only after the isolated runtime is ready.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var timedOut atomic.Bool
	defer func() {
		if recovered := recover(); recovered != nil {
			if timedOut.Load() {
				err = fmt.Errorf("transform timed out after %s", limits.Timeout)
				return
			}
			err = fmt.Errorf("transform runtime failed: %v", recovered)
		}
	}()

	var stderr bytes.Buffer
	runtime, err := qjs.New(qjs.Option{
		Context: ctx, CloseOnContextDone: true, MemoryLimit: limits.MemoryBytes,
		MaxStackSize: limits.StackBytes, Stdout: &bytes.Buffer{}, Stderr: &stderr,
	})
	if err != nil {
		return Result{}, fmt.Errorf("start transform runtime: %w", err)
	}
	defer runtime.Close()
	timer := time.AfterFunc(limits.Timeout, func() {
		timedOut.Store(true)
		cancel()
	})
	defer timer.Stop()

	code := lockedPrelude + "\n" + normalizeSource(source)
	evaluated, err := runtime.Eval("transform.js", qjs.Code(code))
	if err != nil {
		if timedOut.Load() {
			return Result{}, fmt.Errorf("transform timed out after %s", limits.Timeout)
		}
		return Result{}, fmt.Errorf("compile transform: %w", err)
	}
	evaluated.Free()
	global := runtime.Context().Global()
	defer global.Free()
	transform := global.GetPropertyStr("transform")
	defer transform.Free()
	if !transform.IsFunction() {
		return Result{}, errors.New("transform source must define transform(context)")
	}
	if validateOnly {
		return Result{Matched: true}, nil
	}

	encoded, err := json.Marshal(input)
	if err != nil {
		return Result{}, fmt.Errorf("encode transform input: %w", err)
	}
	inputValue := runtime.Context().ParseJSON(string(encoded))
	defer inputValue.Free()
	filter := global.GetPropertyStr("filter")
	if filter.IsFunction() {
		filtered, invokeErr := runtime.Context().Invoke(filter, global, inputValue)
		filter.Free()
		if invokeErr != nil {
			return Result{}, fmt.Errorf("run filter: %w", invokeErr)
		}
		matched := filtered.Bool()
		filtered.Free()
		if !matched {
			return Result{Matched: false}, nil
		}
	} else {
		filter.Free()
	}

	output, err := runtime.Context().Invoke(transform, global, inputValue)
	if err != nil {
		return Result{}, fmt.Errorf("run transform: %w", err)
	}
	defer output.Free()
	if output.IsPromise() {
		return Result{}, errors.New("async transform results are not allowed")
	}
	if output.IsUndefined() || output.IsNull() {
		return Result{Matched: false}, nil
	}
	payload, err := output.JSONStringify()
	if err != nil {
		return Result{}, fmt.Errorf("transform result is not JSON serializable: %w", err)
	}
	if limits.PayloadBytes <= 0 {
		limits.PayloadBytes = DefaultLimits().PayloadBytes
	}
	if len(payload) > limits.PayloadBytes {
		return Result{}, fmt.Errorf("transform payload exceeds %d bytes", limits.PayloadBytes)
	}
	if !json.Valid([]byte(payload)) {
		return Result{}, errors.New("transform returned invalid JSON")
	}
	return Result{Matched: true, Payload: json.RawMessage(payload)}, nil
}

func validateSource(source string, limits Limits) error {
	if limits.SourceBytes <= 0 {
		limits.SourceBytes = DefaultLimits().SourceBytes
	}
	if source == "" || len(source) > limits.SourceBytes {
		return fmt.Errorf("transform source must be between 1 and %d bytes", limits.SourceBytes)
	}
	for _, expression := range forbidden {
		if expression.MatchString(source) {
			return fmt.Errorf("transform uses forbidden syntax %q", expression.String())
		}
	}
	return nil
}

func normalizeSource(source string) string {
	return exportFunction.ReplaceAllStringFunc(source, func(match string) string {
		name := "filter"
		if strings.Contains(match, "transform") {
			name = "transform"
		}
		return "globalThis." + name + " = function("
	})
}
