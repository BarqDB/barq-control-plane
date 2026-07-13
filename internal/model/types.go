package model

import "time"

// Scope identifies one customer's logical Barq database.
type Scope struct {
	Tenant   string `json:"tenant"`
	Database string `json:"database"`
}

func (s Scope) Valid() bool { return s.Tenant != "" && s.Database != "" }

// Object is the public JSON form of one Barq object.
type Object struct {
	Type       string         `json:"type"`
	PrimaryKey any            `json:"primary_key"`
	Data       map[string]any `json:"data"`
	ETag       string         `json:"etag"`
}

// ChangeEvent is the durable, ordered form consumed by webhook workers.
type ChangeEvent struct {
	ID          string         `json:"id"`
	Scope       Scope          `json:"scope"`
	Cursor      uint64         `json:"cursor"`
	Snapshot    uint64         `json:"snapshot"`
	Type        string         `json:"type"`
	ObjectType  string         `json:"object_type"`
	PrimaryKey  any            `json:"primary_key"`
	Source      string         `json:"source"`
	Changed     []string       `json:"changed_fields,omitempty"`
	Before      map[string]any `json:"before,omitempty"`
	After       map[string]any `json:"after,omitempty"`
	CommittedAt time.Time      `json:"committed_at"`
}

// EventContext is the immutable input passed to a webhook transform.
type EventContext struct {
	Event   ChangeEvent    `json:"event"`
	Before  map[string]any `json:"before,omitempty"`
	After   map[string]any `json:"after,omitempty"`
	Related map[string]any `json:"related,omitempty"`
}
