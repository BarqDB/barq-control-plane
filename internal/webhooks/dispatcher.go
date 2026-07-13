package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/barqdb/barq-server/internal/control"
	"github.com/barqdb/barq-server/internal/dataplane"
	"github.com/barqdb/barq-server/internal/transforms"
)

type Dispatcher struct {
	data    dataplane.DataPlane
	store   control.Store
	runtime transforms.Runtime
	client  *http.Client
	now     func() time.Time
}

func NewDispatcher(data dataplane.DataPlane, store control.Store, runtime transforms.Runtime, client *http.Client) *Dispatcher {
	if client == nil {
		client = &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Dispatcher{data: data, store: store, runtime: runtime, client: client, now: func() time.Time { return time.Now().UTC() }}
}

func (d *Dispatcher) PollOnce(ctx context.Context, scope dataplane.Scope) (int, error) {
	cursor, cursorVersion, err := d.loadCursor(ctx, scope)
	if err != nil {
		return 0, err
	}
	changes, err := d.data.ReadChanges(ctx, dataplane.ChangesRequest{Scope: scope, After: cursor, Limit: 100})
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, event := range changes.Events {
		if err := d.processEvent(ctx, event); err != nil {
			return processed, err
		}
		cursor = event.Cursor
		if err := d.saveCursor(ctx, scope, cursor, cursorVersion); err != nil {
			return processed, err
		}
		cursorVersion++
		processed++
	}
	return processed, nil
}

func (d *Dispatcher) processEvent(ctx context.Context, event dataplane.ChangeEvent) error {
	records, err := d.store.List(ctx, CollectionWebhooks, "")
	if err != nil {
		return err
	}
	for _, record := range records {
		hook, err := control.Decode[Webhook](record)
		if err != nil {
			return err
		}
		if !matchesHook(hook, event) {
			continue
		}
		revision, err := d.loadRevision(ctx, hook.ID, hook.ActiveRevision)
		if err != nil {
			return err
		}
		materializationID := fmt.Sprintf("%s/%020d/%s", hook.ID, revision.Number, event.ID)
		materialization, version, err := d.loadOrMaterialize(ctx, materializationID, hook, revision, event)
		if err != nil {
			return err
		}
		switch materialization.Status {
		case "stored":
			if err := d.transformMaterialization(ctx, hook, revision, materialization, version); err != nil {
				return err
			}
		case "rendered", "dead":
			if err := d.ensureDelivery(ctx, hook, revision, materialization); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Dispatcher) loadOrMaterialize(ctx context.Context, id string, hook Webhook, revision Revision, event dataplane.ChangeEvent) (Materialization, uint64, error) {
	record, err := d.store.Get(ctx, CollectionMaterialized, id)
	if err == nil {
		value, decodeErr := control.Decode[Materialization](record)
		return value, record.Version, decodeErr
	}
	if !dataplane.IsCode(err, dataplane.CodeNotFound) {
		return Materialization{}, 0, err
	}
	eventContext, err := d.data.MaterializeEvent(ctx, dataplane.MaterializeRequest{Scope: event.Scope, EventID: event.ID, Reads: hook.Reads})
	if err != nil {
		return Materialization{}, 0, err
	}
	materialization := Materialization{
		ID: id, WebhookID: hook.ID, Revision: revision.Number, EventID: event.ID,
		Context: eventContext, Status: "stored", CreatedAt: d.now(),
	}
	encoded, _ := control.Encode(materialization)
	zero := uint64(0)
	created, err := d.store.Put(ctx, CollectionMaterialized, id, encoded, &zero)
	if dataplane.IsCode(err, dataplane.CodeConflict) {
		existing, getErr := d.store.Get(ctx, CollectionMaterialized, id)
		if getErr != nil {
			return Materialization{}, 0, getErr
		}
		value, decodeErr := control.Decode[Materialization](existing)
		return value, existing.Version, decodeErr
	}
	return materialization, created.Version, err
}

func (d *Dispatcher) transformMaterialization(ctx context.Context, hook Webhook, revision Revision, materialization Materialization, version uint64) error {
	result, err := d.runtime.Execute(ctx, revision.Source, materialization.Context, revision.Limits)
	now := d.now()
	materialization.TransformedAt = &now
	if err != nil {
		materialization.Status, materialization.Error = "dead", err.Error()
		if err := d.updateMaterialization(ctx, materialization, version); err != nil {
			return err
		}
		delivery := Delivery{
			ID: deliveryID(hook.ID, revision.Number, materialization.EventID), WebhookID: hook.ID,
			Revision: revision.Number, EventID: materialization.EventID, URL: hook.URL,
			Status: "dead", Stage: "transform", LastError: err.Error(), CreatedAt: now, NextAttemptAt: now,
		}
		return d.createDelivery(ctx, delivery)
	}
	if !result.Matched {
		materialization.Status = "skipped"
		return d.updateMaterialization(ctx, materialization, version)
	}
	materialization.Status = "rendered"
	materialization.Payload = append(json.RawMessage(nil), result.Payload...)
	if err := d.updateMaterialization(ctx, materialization, version); err != nil {
		return err
	}
	delivery := Delivery{
		ID: deliveryID(hook.ID, revision.Number, materialization.EventID), WebhookID: hook.ID,
		Revision: revision.Number, EventID: materialization.EventID, URL: hook.URL,
		Payload: append(json.RawMessage(nil), result.Payload...), Status: "pending", Stage: "delivery",
		CreatedAt: now, NextAttemptAt: now,
	}
	return d.createDelivery(ctx, delivery)
}

func (d *Dispatcher) ensureDelivery(ctx context.Context, hook Webhook, revision Revision, materialization Materialization) error {
	now := d.now()
	delivery := Delivery{
		ID: deliveryID(hook.ID, revision.Number, materialization.EventID), WebhookID: hook.ID,
		Revision: revision.Number, EventID: materialization.EventID, URL: hook.URL,
		Payload: append(json.RawMessage(nil), materialization.Payload...), Status: "pending", Stage: "delivery",
		CreatedAt: now, NextAttemptAt: now,
	}
	if materialization.Status == "dead" {
		delivery.Status, delivery.Stage, delivery.LastError = "dead", "transform", materialization.Error
	}
	return d.createDelivery(ctx, delivery)
}

func (d *Dispatcher) DeliverDue(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	records, err := d.store.List(ctx, CollectionDeliveries, "")
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, record := range records {
		if delivered == limit {
			break
		}
		delivery, err := control.Decode[Delivery](record)
		if err != nil {
			return delivered, err
		}
		if (delivery.Status != "pending" && delivery.Status != "retry") || delivery.NextAttemptAt.After(d.now()) {
			continue
		}
		revision, err := d.loadRevision(ctx, delivery.WebhookID, delivery.Revision)
		if err != nil {
			return delivered, err
		}
		d.deliver(ctx, &delivery, revision)
		encoded, _ := control.Encode(delivery)
		if _, err := d.store.Put(ctx, CollectionDeliveries, record.Key, encoded, &record.Version); err != nil {
			return delivered, err
		}
		delivered++
	}
	return delivered, nil
}

func (d *Dispatcher) deliver(ctx context.Context, delivery *Delivery, revision Revision) {
	delivery.Attempts++
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, delivery.URL, bytes.NewReader(delivery.Payload))
	if err == nil {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("User-Agent", "Barq-Webhook/1")
		request.Header.Set("X-Barq-Event-ID", delivery.EventID)
		request.Header.Set("X-Barq-Webhook-ID", delivery.WebhookID)
		request.Header.Set("X-Barq-Delivery-ID", delivery.ID)
		request.Header.Set("X-Barq-Signature", signature(revision.SigningSecret, delivery.EventID, delivery.Payload))
		var response *http.Response
		response, err = d.client.Do(request)
		if response != nil {
			delivery.LastStatus = response.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				now := d.now()
				delivery.Status, delivery.LastError, delivery.CompletedAt = "completed", "", &now
				return
			}
			err = fmt.Errorf("webhook returned HTTP %d", response.StatusCode)
		}
	}
	if err != nil {
		delivery.LastError = err.Error()
	}
	if delivery.Attempts >= 8 {
		delivery.Status = "dead"
		return
	}
	delivery.Status = "retry"
	delivery.NextAttemptAt = d.now().Add(backoff(delivery.Attempts))
}

func (d *Dispatcher) loadRevision(ctx context.Context, hookID string, number uint64) (Revision, error) {
	record, err := d.store.Get(ctx, CollectionRevisions, revisionKey(hookID, number))
	if err != nil {
		return Revision{}, err
	}
	return control.Decode[Revision](record)
}

func (d *Dispatcher) createDelivery(ctx context.Context, delivery Delivery) error {
	encoded, _ := control.Encode(delivery)
	zero := uint64(0)
	_, err := d.store.Put(ctx, CollectionDeliveries, delivery.ID, encoded, &zero)
	if dataplane.IsCode(err, dataplane.CodeConflict) {
		return nil
	}
	return err
}

func (d *Dispatcher) updateMaterialization(ctx context.Context, materialization Materialization, version uint64) error {
	encoded, _ := control.Encode(materialization)
	_, err := d.store.Put(ctx, CollectionMaterialized, materialization.ID, encoded, &version)
	return err
}

func (d *Dispatcher) loadCursor(ctx context.Context, scope dataplane.Scope) (uint64, uint64, error) {
	record, err := d.store.Get(ctx, CollectionChangeCursors, cursorKey(scope))
	if dataplane.IsCode(err, dataplane.CodeNotFound) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	cursor, err := control.Decode[Cursor](record)
	return cursor.Cursor, record.Version, err
}

func (d *Dispatcher) saveCursor(ctx context.Context, scope dataplane.Scope, cursor, priorVersion uint64) error {
	encoded, _ := control.Encode(Cursor{Scope: scope, Cursor: cursor})
	_, err := d.store.Put(ctx, CollectionChangeCursors, cursorKey(scope), encoded, &priorVersion)
	return err
}

func cursorKey(scope dataplane.Scope) string { return scope.Tenant + "/" + scope.Database }

func deliveryID(webhookID string, revision uint64, eventID string) string {
	return webhookID + "/" + fmt.Sprintf("%020d", revision) + "/" + eventID
}

func signature(secret, eventID string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(eventID))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func backoff(attempt int) time.Duration {
	seconds := 1 << min(attempt, 12)
	if seconds > 3600 {
		seconds = 3600
	}
	return time.Duration(seconds) * time.Second
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = strconv.Itoa
