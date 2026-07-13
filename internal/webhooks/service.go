package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/barqdb/barq-server/internal/control"
	"github.com/barqdb/barq-server/internal/dataplane"
	"github.com/barqdb/barq-server/internal/transforms"
)

type Service struct {
	store            control.Store
	runtime          transforms.Runtime
	now              func() time.Time
	allowPrivateURLs bool
}

func NewService(store control.Store, runtime transforms.Runtime, allowPrivateURLs bool) *Service {
	return &Service{store: store, runtime: runtime, now: func() time.Time { return time.Now().UTC() }, allowPrivateURLs: allowPrivateURLs}
}

func (s *Service) Register(ctx context.Context, input Registration) (Registered, error) {
	if err := s.validate(ctx, input); err != nil {
		return Registered{}, err
	}
	id, err := randomHex(16)
	if err != nil {
		return Registered{}, err
	}
	secret, err := randomSecret()
	if err != nil {
		return Registered{}, err
	}
	now := s.now()
	hook := Webhook{
		ID: id, Name: input.Name, Scope: input.Scope, URL: input.URL, Events: uniqueSorted(input.Events),
		ObjectTypes: uniqueSorted(input.ObjectTypes), Reads: input.Reads, ActiveRevision: 1,
		Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	revision := Revision{WebhookID: id, Number: 1, Source: input.Transform.Source, Limits: effectiveLimits(input.Limits), SigningSecret: secret, CreatedAt: now}
	hookValue, err := control.Encode(hook)
	if err != nil {
		return Registered{}, err
	}
	revisionValue, err := control.Encode(revision)
	if err != nil {
		return Registered{}, err
	}
	zero := uint64(0)
	if _, err := s.store.Apply(ctx, []control.Mutation{
		{Op: control.MutationPut, Collection: CollectionWebhooks, Key: id, Value: hookValue, ExpectedVersion: &zero},
		{Op: control.MutationPut, Collection: CollectionRevisions, Key: revisionKey(id, 1), Value: revisionValue, ExpectedVersion: &zero},
	}); err != nil {
		return Registered{}, err
	}
	return Registered{Webhook: hook, Secret: secret}, nil
}

func (s *Service) Get(ctx context.Context, id string) (Webhook, error) {
	record, err := s.store.Get(ctx, CollectionWebhooks, id)
	if err != nil {
		return Webhook{}, err
	}
	return control.Decode[Webhook](record)
}

func (s *Service) List(ctx context.Context, scope *dataplane.Scope) ([]Webhook, error) {
	records, err := s.store.List(ctx, CollectionWebhooks, "")
	if err != nil {
		return nil, err
	}
	hooks := make([]Webhook, 0, len(records))
	for _, record := range records {
		hook, decodeErr := control.Decode[Webhook](record)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if scope == nil || hook.Scope == *scope {
			hooks = append(hooks, hook)
		}
	}
	sort.Slice(hooks, func(i, j int) bool { return hooks[i].CreatedAt.Before(hooks[j].CreatedAt) })
	return hooks, nil
}

func (s *Service) Update(ctx context.Context, id string, input Registration) (Webhook, error) {
	if err := s.validate(ctx, input); err != nil {
		return Webhook{}, err
	}
	record, err := s.store.Get(ctx, CollectionWebhooks, id)
	if err != nil {
		return Webhook{}, err
	}
	hook, err := control.Decode[Webhook](record)
	if err != nil {
		return Webhook{}, err
	}
	prior, err := s.revision(ctx, id, hook.ActiveRevision)
	if err != nil {
		return Webhook{}, err
	}
	now := s.now()
	hook.Name, hook.Scope, hook.URL = input.Name, input.Scope, input.URL
	hook.Events, hook.ObjectTypes, hook.Reads = uniqueSorted(input.Events), uniqueSorted(input.ObjectTypes), input.Reads
	hook.ActiveRevision++
	hook.Enabled, hook.UpdatedAt = true, now
	revision := Revision{WebhookID: id, Number: hook.ActiveRevision, Source: input.Transform.Source, Limits: effectiveLimits(input.Limits), SigningSecret: prior.SigningSecret, CreatedAt: now}
	revisionValue, err := control.Encode(revision)
	if err != nil {
		return Webhook{}, err
	}
	hookValue, err := control.Encode(hook)
	if err != nil {
		return Webhook{}, err
	}
	zero := uint64(0)
	if _, err := s.store.Apply(ctx, []control.Mutation{
		{Op: control.MutationPut, Collection: CollectionRevisions, Key: revisionKey(id, revision.Number), Value: revisionValue, ExpectedVersion: &zero},
		{Op: control.MutationPut, Collection: CollectionWebhooks, Key: id, Value: hookValue, ExpectedVersion: &record.Version},
	}); err != nil {
		return Webhook{}, err
	}
	return hook, nil
}

func (s *Service) Disable(ctx context.Context, id string) error {
	record, err := s.store.Get(ctx, CollectionWebhooks, id)
	if err != nil {
		return err
	}
	hook, err := control.Decode[Webhook](record)
	if err != nil {
		return err
	}
	hook.Enabled = false
	hook.UpdatedAt = s.now()
	encoded, _ := control.Encode(hook)
	_, err = s.store.Put(ctx, CollectionWebhooks, id, encoded, &record.Version)
	return err
}

func (s *Service) RotateSecret(ctx context.Context, id string) (string, error) {
	record, err := s.store.Get(ctx, CollectionWebhooks, id)
	if err != nil {
		return "", err
	}
	hook, err := control.Decode[Webhook](record)
	if err != nil {
		return "", err
	}
	prior, err := s.revision(ctx, id, hook.ActiveRevision)
	if err != nil {
		return "", err
	}
	secret, err := randomSecret()
	if err != nil {
		return "", err
	}
	hook.ActiveRevision++
	hook.UpdatedAt = s.now()
	revision := prior
	revision.Number, revision.SigningSecret, revision.CreatedAt = hook.ActiveRevision, secret, s.now()
	revisionValue, err := control.Encode(revision)
	if err != nil {
		return "", err
	}
	hookValue, err := control.Encode(hook)
	if err != nil {
		return "", err
	}
	zero := uint64(0)
	if _, err := s.store.Apply(ctx, []control.Mutation{
		{Op: control.MutationPut, Collection: CollectionRevisions, Key: revisionKey(id, revision.Number), Value: revisionValue, ExpectedVersion: &zero},
		{Op: control.MutationPut, Collection: CollectionWebhooks, Key: id, Value: hookValue, ExpectedVersion: &record.Version},
	}); err != nil {
		return "", err
	}
	return secret, nil
}

func (s *Service) Test(ctx context.Context, id string, eventContext dataplane.EventContext) (transforms.Result, error) {
	hook, err := s.Get(ctx, id)
	if err != nil {
		return transforms.Result{}, err
	}
	revision, err := s.revision(ctx, id, hook.ActiveRevision)
	if err != nil {
		return transforms.Result{}, err
	}
	return s.runtime.Execute(ctx, revision.Source, eventContext, revision.Limits)
}

func (s *Service) Replay(ctx context.Context, id string) (int, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return 0, err
	}
	records, err := s.store.List(ctx, CollectionDeliveries, id+"/")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, record := range records {
		delivery, decodeErr := control.Decode[Delivery](record)
		if decodeErr != nil {
			return count, decodeErr
		}
		if delivery.Status != "dead" {
			continue
		}
		delivery.Status, delivery.Stage, delivery.Attempts = "pending", "delivery", 0
		delivery.LastError, delivery.LastStatus, delivery.NextAttemptAt = "", 0, s.now()
		encoded, _ := control.Encode(delivery)
		if _, err := s.store.Put(ctx, CollectionDeliveries, record.Key, encoded, &record.Version); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func (s *Service) OperationalHealth(ctx context.Context, visible func(dataplane.Scope) bool) (OperationalHealth, error) {
	health := OperationalHealth{Status: "ok"}
	hooks, err := s.List(ctx, nil)
	if err != nil {
		return health, err
	}
	for _, hook := range hooks {
		if visible != nil && !visible(hook.Scope) {
			continue
		}
		records, err := s.store.List(ctx, CollectionDeliveries, hook.ID+"/")
		if err != nil {
			return health, err
		}
		for _, record := range records {
			delivery, err := control.Decode[Delivery](record)
			if err != nil {
				return health, err
			}
			switch delivery.Status {
			case "pending":
				health.Pending++
				health.OldestPendingAt = earlierTime(health.OldestPendingAt, delivery.CreatedAt)
			case "retry":
				health.Retrying++
				health.OldestPendingAt = earlierTime(health.OldestPendingAt, delivery.CreatedAt)
			case "dead":
				if delivery.Stage == "transform" {
					health.DeadTransform++
				} else {
					health.DeadDelivery++
				}
			}
		}
	}
	return health, nil
}

func earlierTime(current *time.Time, candidate time.Time) *time.Time {
	if candidate.IsZero() || (current != nil && !candidate.Before(*current)) {
		return current
	}
	value := candidate
	return &value
}

func (s *Service) revision(ctx context.Context, id string, number uint64) (Revision, error) {
	record, err := s.store.Get(ctx, CollectionRevisions, revisionKey(id, number))
	if err != nil {
		return Revision{}, err
	}
	return control.Decode[Revision](record)
}

func (s *Service) validate(ctx context.Context, input Registration) error {
	if input.Name == "" || !input.Scope.Valid() || len(input.Events) == 0 || input.Transform.Source == "" {
		return &dataplane.Error{Code: dataplane.CodeInvalid, Message: "name, scope, events, and transform source are required"}
	}
	if input.Transform.Language != "javascript" {
		return &dataplane.Error{Code: dataplane.CodeInvalid, Message: "transform language must be javascript"}
	}
	if len(input.Reads) > 5 {
		return &dataplane.Error{Code: dataplane.CodeInvalid, Message: "a webhook may define at most five related reads"}
	}
	aliases := map[string]bool{}
	for _, read := range input.Reads {
		if read.As == "" || read.From == "" || read.Where.Field == "" || read.Where.Op == "" {
			return &dataplane.Error{Code: dataplane.CodeInvalid, Message: "each related read needs as, from, and where"}
		}
		if aliases[read.As] {
			return &dataplane.Error{Code: dataplane.CodeInvalid, Message: "related read aliases must be unique"}
		}
		aliases[read.As] = true
		if read.Limit > 100 {
			return &dataplane.Error{Code: dataplane.CodeInvalid, Message: "related read limit cannot exceed 100"}
		}
	}
	parsed, err := url.Parse(input.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return &dataplane.Error{Code: dataplane.CodeInvalid, Message: "valid webhook URL is required"}
	}
	if parsed.Scheme != "https" && !s.allowPrivateURLs {
		return &dataplane.Error{Code: dataplane.CodeInvalid, Message: "webhook URL must use HTTPS"}
	}
	if !s.allowPrivateURLs && isPrivateHost(parsed.Hostname()) {
		return &dataplane.Error{Code: dataplane.CodeInvalid, Message: "private webhook targets are disabled"}
	}
	if err := s.runtime.Validate(ctx, input.Transform.Source, effectiveLimits(input.Limits)); err != nil {
		return &dataplane.Error{Code: dataplane.CodeInvalid, Message: err.Error()}
	}
	return nil
}

func effectiveLimits(input transforms.Limits) transforms.Limits {
	defaults := transforms.DefaultLimits()
	if input.SourceBytes <= 0 || input.SourceBytes > defaults.SourceBytes {
		input.SourceBytes = defaults.SourceBytes
	}
	if input.MemoryBytes <= 0 || input.MemoryBytes > defaults.MemoryBytes {
		input.MemoryBytes = defaults.MemoryBytes
	}
	if input.StackBytes <= 0 || input.StackBytes > defaults.StackBytes {
		input.StackBytes = defaults.StackBytes
	}
	if input.Timeout <= 0 || input.Timeout > defaults.Timeout {
		input.Timeout = defaults.Timeout
	}
	if input.PayloadBytes <= 0 || input.PayloadBytes > defaults.PayloadBytes {
		input.PayloadBytes = defaults.PayloadBytes
	}
	return input
}

func revisionKey(id string, number uint64) string { return fmt.Sprintf("%s/%020d", id, number) }

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func randomHex(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func isPrivateHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified())
}

func matchesHook(hook Webhook, event dataplane.ChangeEvent) bool {
	if !hook.Enabled || hook.Scope != event.Scope {
		return false
	}
	if !contains(hook.Events, "*") && !contains(hook.Events, event.Type) {
		return false
	}
	return len(hook.ObjectTypes) == 0 || contains(hook.ObjectTypes, event.ObjectType)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

var _ = errors.Is
