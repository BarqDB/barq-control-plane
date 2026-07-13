package dataplane

import (
	"encoding/json"

	"github.com/barqdb/barq-server/internal/model"
)

type Scope = model.Scope
type Object = model.Object

type ReadRequest struct {
	RequestID  string `json:"request_id,omitempty"`
	Scope      Scope  `json:"scope"`
	Type       string `json:"type"`
	PrimaryKey any    `json:"primary_key"`
}

type WriteOperation string

const (
	WriteCreate WriteOperation = "create"
	WritePatch  WriteOperation = "patch"
	WriteDelete WriteOperation = "delete"
)

type WriteRequest struct {
	RequestID      string         `json:"request_id,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	Scope          Scope          `json:"scope"`
	Operation      WriteOperation `json:"operation"`
	Type           string         `json:"type"`
	PrimaryKey     any            `json:"primary_key"`
	Data           map[string]any `json:"data,omitempty"`
	IfMatch        string         `json:"if_match,omitempty"`
}

type WriteResult struct {
	Object  *Object `json:"object,omitempty"`
	Deleted bool    `json:"deleted,omitempty"`
	Cursor  uint64  `json:"cursor"`
}

type Filter struct {
	Field    string   `json:"field,omitempty"`
	Op       string   `json:"op,omitempty"`
	Value    any      `json:"value,omitempty"`
	Children []Filter `json:"children,omitempty"`
}

type Sort struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc,omitempty"`
}

type QueryRequest struct {
	RequestID string  `json:"request_id,omitempty"`
	Scope     Scope   `json:"scope"`
	Type      string  `json:"type"`
	Filter    *Filter `json:"filter,omitempty"`
	Sort      []Sort  `json:"sort,omitempty"`
	Limit     int     `json:"limit,omitempty"`
	Cursor    string  `json:"cursor,omitempty"`
}

type QueryResult struct {
	Objects    []Object `json:"objects"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

type BatchRequest struct {
	RequestID      string         `json:"request_id,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	Scope          Scope          `json:"scope"`
	Operations     []WriteRequest `json:"operations"`
}

type BatchResult struct {
	Results []WriteResult `json:"results"`
}

type SchemaRequest struct {
	RequestID string          `json:"request_id,omitempty"`
	Scope     Scope           `json:"scope"`
	Version   uint64          `json:"version"`
	Manifest  json.RawMessage `json:"manifest"`
}

type SchemaResult struct {
	CurrentVersion uint64   `json:"current_version"`
	TargetVersion  uint64   `json:"target_version"`
	Changes        []string `json:"changes,omitempty"`
	Applied        bool     `json:"applied"`
}

type ChangeEvent = model.ChangeEvent

type ChangesRequest struct {
	Scope  Scope  `json:"scope"`
	After  uint64 `json:"after"`
	Limit  int    `json:"limit,omitempty"`
	WaitMS int    `json:"wait_ms,omitempty"`
}

type ChangesResult struct {
	Events     []ChangeEvent `json:"events"`
	NextCursor uint64        `json:"next_cursor"`
}

type RefValue struct {
	Ref string `json:"$ref"`
}

type RelatedRead struct {
	As    string `json:"as"`
	From  string `json:"from"`
	Where Filter `json:"where"`
	Sort  []Sort `json:"sort,omitempty"`
	Limit int    `json:"limit,omitempty"`
	One   bool   `json:"one,omitempty"`
}

type MaterializeRequest struct {
	Scope   Scope         `json:"scope"`
	EventID string        `json:"event_id"`
	Reads   []RelatedRead `json:"reads,omitempty"`
}

type EventContext = model.EventContext

type Health struct {
	Status       string   `json:"status"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
}
