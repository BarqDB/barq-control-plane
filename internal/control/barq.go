package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	barq "github.com/BarqDB/barq-go"
	"github.com/barqdb/barq-server/internal/dataplane"
)

const controlClass = "ControlRecord"

var controlSchema = barq.Schema{{
	Name:       controlClass,
	PrimaryKey: "id",
	Properties: []barq.Property{
		{Name: "id", Type: barq.TypeString},
		{Name: "collection", Type: barq.TypeString, Indexed: true},
		{Name: "key", Type: barq.TypeString, Indexed: true},
		{Name: "value", Type: barq.TypeBinary},
		{Name: "version", Type: barq.TypeInt},
	},
}}

// BarqStore stores control-plane records directly in a local Barq database.
// The mutex makes each read-check-write sequence atomic inside this process;
// barq-go serializes the actual native database operations on one OS thread.
type BarqStore struct {
	db *barq.DB
	mu sync.Mutex
}

func OpenBarqStore(path string) (*BarqStore, error) {
	if path == "" {
		return nil, errors.New("control Barq path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create control directory: %w", err)
	}
	db, err := barq.Open(barq.Config{Path: path, Schema: controlSchema, SchemaVersion: 1})
	if err != nil {
		return nil, fmt.Errorf("open control Barq: %w", err)
	}
	return &BarqStore{db: db}, nil
}

func (s *BarqStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *BarqStore) Get(ctx context.Context, collection, key string) (Record, error) {
	if err := validateIdentity(collection, key); err != nil {
		return Record{}, err
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(collection, key)
}

func (s *BarqStore) List(ctx context.Context, collection, prefix string) ([]Record, error) {
	if collection == "" {
		return nil, invalid("collection is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	results, err := s.db.All(controlClass)
	if err != nil {
		return nil, mapBarqError(err)
	}
	defer results.Close()
	count, err := results.Len()
	if err != nil {
		return nil, mapBarqError(err)
	}
	records := make([]Record, 0)
	for index := 0; index < count; index++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		object, err := results.At(index)
		if err != nil {
			return nil, mapBarqError(err)
		}
		record, readErr := readRecord(object)
		closeErr := object.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, mapBarqError(closeErr)
		}
		if record.Collection == collection && len(record.Key) >= len(prefix) && record.Key[:len(prefix)] == prefix {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Key < records[j].Key })
	return records, nil
}

func (s *BarqStore) Put(ctx context.Context, collection, key string, value json.RawMessage, expected *uint64) (Record, error) {
	records, err := s.Apply(ctx, []Mutation{{Op: MutationPut, Collection: collection, Key: key, Value: value, ExpectedVersion: expected}})
	if err != nil {
		return Record{}, err
	}
	return records[0], nil
}

func (s *BarqStore) Delete(ctx context.Context, collection, key string, expected *uint64) error {
	_, err := s.Apply(ctx, []Mutation{{Op: MutationDelete, Collection: collection, Key: key, ExpectedVersion: expected}})
	return err
}

type pendingMutation struct {
	mutation Mutation
	object   *barq.Object
	found    bool
	next     uint64
}

func (s *BarqStore) Apply(ctx context.Context, mutations []Mutation) ([]Record, error) {
	if len(mutations) == 0 || len(mutations) > 100 {
		return nil, invalid("a control transaction needs 1 to 100 mutations")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pending := make([]pendingMutation, len(mutations))
	seen := make(map[string]bool, len(mutations))
	closePending := func() {
		for i := range pending {
			if pending[i].object != nil {
				_ = pending[i].object.Close()
			}
		}
	}
	defer closePending()

	for i, mutation := range mutations {
		if err := validateIdentity(mutation.Collection, mutation.Key); err != nil {
			return nil, err
		}
		if mutation.Op != MutationPut && mutation.Op != MutationDelete {
			return nil, invalid("unknown control mutation")
		}
		if mutation.Op == MutationPut && !json.Valid(mutation.Value) {
			return nil, invalid("value must be valid JSON")
		}
		id := recordID(mutation.Collection, mutation.Key)
		if seen[id] {
			return nil, invalid("a control transaction cannot mutate the same record twice")
		}
		seen[id] = true
		object, found, err := s.db.Find(controlClass, id)
		if err != nil {
			return nil, mapBarqError(err)
		}
		current := uint64(0)
		if found {
			version, err := object.Get("version")
			if err != nil {
				_ = object.Close()
				return nil, mapBarqError(err)
			}
			current = uint64(version.(int64))
		}
		pending[i] = pendingMutation{mutation: mutation, object: object, found: found, next: current + 1}
		if mutation.Op == MutationDelete && !found {
			return nil, notFound()
		}
		if expected := mutation.ExpectedVersion; expected != nil &&
			((*expected == 0 && found) || (*expected != 0 && (!found || current != *expected))) {
			return nil, conflict("control record version mismatch")
		}
	}

	created := make([]*barq.Object, 0)
	err := s.db.Write(func(tx *barq.Tx) error {
		for i := range pending {
			item := &pending[i]
			if item.mutation.Op == MutationDelete {
				if err := tx.Delete(item.object); err != nil {
					return err
				}
				item.object = nil
				continue
			}
			stored := append([]byte(nil), item.mutation.Value...)
			if item.found {
				if err := tx.Set(item.object, "value", stored); err != nil {
					return err
				}
				if err := tx.Set(item.object, "version", int64(item.next)); err != nil {
					return err
				}
				continue
			}
			object, err := tx.Create(controlClass, map[string]any{
				"id":         recordID(item.mutation.Collection, item.mutation.Key),
				"collection": item.mutation.Collection, "key": item.mutation.Key,
				"value": stored, "version": int64(item.next),
			})
			if err != nil {
				return err
			}
			created = append(created, object)
		}
		return nil
	})
	for _, object := range created {
		_ = object.Close()
	}
	if err != nil {
		return nil, mapBarqError(err)
	}
	records := make([]Record, 0, len(mutations))
	for _, item := range pending {
		if item.mutation.Op == MutationPut {
			records = append(records, Record{Collection: item.mutation.Collection, Key: item.mutation.Key,
				Value: append(json.RawMessage(nil), item.mutation.Value...), Version: item.next})
		}
	}
	return records, nil
}

func (s *BarqStore) getLocked(collection, key string) (Record, error) {
	object, found, err := s.db.Find(controlClass, recordID(collection, key))
	if err != nil {
		return Record{}, mapBarqError(err)
	}
	if !found {
		return Record{}, notFound()
	}
	defer object.Close()
	return readRecord(object)
}

func readRecord(object *barq.Object) (Record, error) {
	collection, err := object.Get("collection")
	if err != nil {
		return Record{}, mapBarqError(err)
	}
	key, err := object.Get("key")
	if err != nil {
		return Record{}, mapBarqError(err)
	}
	value, err := object.Get("value")
	if err != nil {
		return Record{}, mapBarqError(err)
	}
	version, err := object.Get("version")
	if err != nil {
		return Record{}, mapBarqError(err)
	}
	return Record{
		Collection: collection.(string), Key: key.(string),
		Value: append(json.RawMessage(nil), value.([]byte)...), Version: uint64(version.(int64)),
	}, nil
}

func validateIdentity(collection, key string) error {
	if collection == "" || key == "" {
		return invalid("collection and key are required")
	}
	return nil
}

func recordID(collection, key string) string {
	return fmt.Sprintf("%d:%s%s", len(collection), collection, key)
}

func invalid(message string) error {
	return &dataplane.Error{Code: dataplane.CodeInvalid, Message: message}
}

func conflict(message string) error {
	return &dataplane.Error{Code: dataplane.CodeConflict, Message: message}
}

func notFound() error {
	return &dataplane.Error{Code: dataplane.CodeNotFound, Message: "control record not found"}
}

func mapBarqError(err error) error {
	if err == nil {
		return nil
	}
	return &dataplane.Error{Code: dataplane.CodeInternal, Message: err.Error()}
}
