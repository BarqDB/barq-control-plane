package webhooks

import (
	"encoding/json"
	"time"

	"github.com/barqdb/barq-server/internal/dataplane"
	"github.com/barqdb/barq-server/internal/transforms"
)

const (
	CollectionWebhooks      = "webhooks"
	CollectionRevisions     = "webhook_revisions"
	CollectionMaterialized  = "webhook_materializations"
	CollectionDeliveries    = "webhook_deliveries"
	CollectionChangeCursors = "webhook_change_cursors"
)

type Webhook struct {
	ID             string                  `json:"id"`
	Name           string                  `json:"name"`
	Scope          dataplane.Scope         `json:"scope"`
	URL            string                  `json:"url"`
	Events         []string                `json:"events"`
	ObjectTypes    []string                `json:"object_types,omitempty"`
	Reads          []dataplane.RelatedRead `json:"reads,omitempty"`
	ActiveRevision uint64                  `json:"active_revision"`
	Enabled        bool                    `json:"enabled"`
	CreatedAt      time.Time               `json:"created_at"`
	UpdatedAt      time.Time               `json:"updated_at"`
}

type Revision struct {
	WebhookID     string            `json:"webhook_id"`
	Number        uint64            `json:"number"`
	Source        string            `json:"source"`
	Limits        transforms.Limits `json:"limits"`
	SigningSecret string            `json:"signing_secret"`
	CreatedAt     time.Time         `json:"created_at"`
}

type Registration struct {
	Name        string                  `json:"name"`
	Scope       dataplane.Scope         `json:"scope"`
	URL         string                  `json:"url"`
	Events      []string                `json:"events"`
	ObjectTypes []string                `json:"object_types,omitempty"`
	Reads       []dataplane.RelatedRead `json:"reads,omitempty"`
	Transform   TransformConfig         `json:"transform"`
	Limits      transforms.Limits       `json:"limits,omitempty"`
}

type TransformConfig struct {
	Language string `json:"language"`
	Source   string `json:"source"`
}

type Registered struct {
	Webhook Webhook `json:"webhook"`
	Secret  string  `json:"secret"`
}

type Materialization struct {
	ID            string                 `json:"id"`
	WebhookID     string                 `json:"webhook_id"`
	Revision      uint64                 `json:"revision"`
	EventID       string                 `json:"event_id"`
	Context       dataplane.EventContext `json:"context"`
	Payload       json.RawMessage        `json:"payload,omitempty"`
	Status        string                 `json:"status"`
	Error         string                 `json:"error,omitempty"`
	CreatedAt     time.Time              `json:"created_at"`
	TransformedAt *time.Time             `json:"transformed_at,omitempty"`
}

type Delivery struct {
	ID            string          `json:"id"`
	WebhookID     string          `json:"webhook_id"`
	Revision      uint64          `json:"revision"`
	EventID       string          `json:"event_id"`
	URL           string          `json:"url"`
	Payload       json.RawMessage `json:"payload"`
	Status        string          `json:"status"`
	Stage         string          `json:"stage"`
	Attempts      int             `json:"attempts"`
	LastStatus    int             `json:"last_status,omitempty"`
	LastError     string          `json:"last_error,omitempty"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	CreatedAt     time.Time       `json:"created_at"`
	CompletedAt   *time.Time      `json:"completed_at,omitempty"`
}

type Cursor struct {
	Scope  dataplane.Scope `json:"scope"`
	Cursor uint64          `json:"cursor"`
}
