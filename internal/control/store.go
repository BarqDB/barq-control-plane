package control

import (
	"context"
	"encoding/json"
)

type Record struct {
	Collection string          `json:"collection"`
	Key        string          `json:"key"`
	Value      json.RawMessage `json:"value"`
	Version    uint64          `json:"version"`
}

type Mutation struct {
	Op              string
	Collection      string
	Key             string
	Value           json.RawMessage
	ExpectedVersion *uint64
}

const (
	MutationPut    = "put"
	MutationDelete = "delete"
)

type Store interface {
	Get(context.Context, string, string) (Record, error)
	List(context.Context, string, string) ([]Record, error)
	Apply(context.Context, []Mutation) ([]Record, error)
	Put(context.Context, string, string, json.RawMessage, *uint64) (Record, error)
	Delete(context.Context, string, string, *uint64) error
}

func Encode(value any) (json.RawMessage, error) { return json.Marshal(value) }

func Decode[T any](record Record) (T, error) {
	var value T
	err := json.Unmarshal(record.Value, &value)
	return value, err
}
