package control

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/barqdb/barq-server/internal/dataplane"
)

const CurrentSchemaVersion = 1

const (
	systemCollection = "_system"
	schemaStateKey   = "control_schema"
)

type SchemaState struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

func CheckMigration(from, to int) error {
	if from <= 0 || to <= 0 {
		return fmt.Errorf("control schema versions must be positive")
	}
	if from > CurrentSchemaVersion || to > CurrentSchemaVersion {
		return fmt.Errorf("this server supports control schema version %d, requested %d to %d", CurrentSchemaVersion, from, to)
	}
	if to < from {
		return fmt.Errorf("control schema downgrade from %d to %d is not supported", from, to)
	}
	return nil
}

func EnsureSchema(ctx context.Context, store Store) error {
	record, err := store.Get(ctx, systemCollection, schemaStateKey)
	if dataplane.IsCode(err, dataplane.CodeNotFound) {
		return writeSchemaState(ctx, store, CurrentSchemaVersion, nil)
	}
	if err != nil {
		return err
	}
	var state SchemaState
	if err := json.Unmarshal(record.Value, &state); err != nil {
		return fmt.Errorf("read control schema state: %w", err)
	}
	if err := CheckMigration(state.Version, CurrentSchemaVersion); err != nil {
		return err
	}
	if state.Version == CurrentSchemaVersion {
		return nil
	}
	return writeSchemaState(ctx, store, CurrentSchemaVersion, &record.Version)
}

func ApplyMigration(ctx context.Context, store Store, from, to int) error {
	if err := CheckMigration(from, to); err != nil {
		return err
	}
	record, err := store.Get(ctx, systemCollection, schemaStateKey)
	if dataplane.IsCode(err, dataplane.CodeNotFound) {
		return writeSchemaState(ctx, store, to, nil)
	}
	if err != nil {
		return err
	}
	var state SchemaState
	if err := json.Unmarshal(record.Value, &state); err != nil {
		return fmt.Errorf("read control schema state: %w", err)
	}
	if state.Version != from && state.Version != to {
		return fmt.Errorf("control database is schema %d, expected %d", state.Version, from)
	}
	if state.Version == to {
		return nil
	}
	return writeSchemaState(ctx, store, to, &record.Version)
}

func writeSchemaState(ctx context.Context, store Store, version int, expected *uint64) error {
	value, err := json.Marshal(SchemaState{Version: version, UpdatedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	if expected == nil {
		zero := uint64(0)
		expected = &zero
	}
	_, err = store.Put(ctx, systemCollection, schemaStateKey, value, expected)
	return err
}
