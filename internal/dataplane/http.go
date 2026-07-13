package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type HTTPDataPlane struct {
	baseURL *url.URL
	secret  string
	client  *http.Client
}

func NewHTTPDataPlane(rawURL, secret string, client *http.Client) (*HTTPDataPlane, error) {
	base, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid data-plane URL %q", rawURL)
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &HTTPDataPlane{baseURL: base, secret: secret, client: client}, nil
}

func (h *HTTPDataPlane) Health(ctx context.Context) (Health, error) {
	var result Health
	err := h.do(ctx, http.MethodGet, "/internal/v1/health", nil, &result)
	return result, err
}

func (h *HTTPDataPlane) ReadObject(ctx context.Context, input ReadRequest) (Object, error) {
	var result Object
	err := h.do(ctx, http.MethodPost, "/internal/v1/read", input, &result)
	return result, err
}

func (h *HTTPDataPlane) WriteObject(ctx context.Context, input WriteRequest) (WriteResult, error) {
	var result WriteResult
	err := h.do(ctx, http.MethodPost, "/internal/v1/write", input, &result)
	return result, err
}

func (h *HTTPDataPlane) QueryObjects(ctx context.Context, input QueryRequest) (QueryResult, error) {
	var result QueryResult
	err := h.do(ctx, http.MethodPost, "/internal/v1/query", input, &result)
	return result, err
}

func (h *HTTPDataPlane) ExecuteBatch(ctx context.Context, input BatchRequest) (BatchResult, error) {
	var result BatchResult
	err := h.do(ctx, http.MethodPost, "/internal/v1/batch", input, &result)
	return result, err
}

func (h *HTTPDataPlane) PlanSchema(ctx context.Context, input SchemaRequest) (SchemaResult, error) {
	var result SchemaResult
	err := h.do(ctx, http.MethodPost, "/internal/v1/schema/plan", input, &result)
	return result, err
}

func (h *HTTPDataPlane) ApplySchema(ctx context.Context, input SchemaRequest) (SchemaResult, error) {
	var result SchemaResult
	err := h.do(ctx, http.MethodPost, "/internal/v1/schema/apply", input, &result)
	return result, err
}

func (h *HTTPDataPlane) ReadChanges(ctx context.Context, input ChangesRequest) (ChangesResult, error) {
	query := url.Values{}
	query.Set("tenant", input.Scope.Tenant)
	query.Set("database", input.Scope.Database)
	query.Set("after", strconv.FormatUint(input.After, 10))
	if input.Limit != 0 {
		query.Set("limit", strconv.Itoa(input.Limit))
	}
	if input.WaitMS != 0 {
		query.Set("wait_ms", strconv.Itoa(input.WaitMS))
	}
	var result ChangesResult
	err := h.do(ctx, http.MethodGet, "/internal/v1/changes?"+query.Encode(), nil, &result)
	return result, err
}

func (h *HTTPDataPlane) MaterializeEvent(ctx context.Context, input MaterializeRequest) (EventContext, error) {
	var result EventContext
	err := h.do(ctx, http.MethodPost, "/internal/v1/events/materialize", input, &result)
	return result, err
}

func (h *HTTPDataPlane) do(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode data-plane request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	requestURL := *h.baseURL
	requestURL.Path = strings.TrimRight(h.baseURL.Path, "/") + strings.SplitN(path, "?", 2)[0]
	if parts := strings.SplitN(path, "?", 2); len(parts) == 2 {
		requestURL.RawQuery = parts[1]
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if h.secret != "" {
		request.Header.Set("Authorization", "Bearer "+h.secret)
	}
	response, err := h.client.Do(request)
	if err != nil {
		return &Error{Code: CodeUnavailable, Message: err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var apiError Error
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&apiError); err != nil || apiError.Code == "" {
			return &Error{Code: codeForStatus(response.StatusCode), Message: response.Status}
		}
		return &apiError
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(output); err != nil {
		return fmt.Errorf("decode data-plane response: %w", err)
	}
	return nil
}

func codeForStatus(status int) ErrorCode {
	switch status {
	case http.StatusBadRequest:
		return CodeInvalid
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeConflict
	case http.StatusPreconditionFailed:
		return CodePrecondition
	case http.StatusTooManyRequests:
		return CodeResourceExceeded
	case http.StatusServiceUnavailable:
		return CodeUnavailable
	default:
		return CodeInternal
	}
}
