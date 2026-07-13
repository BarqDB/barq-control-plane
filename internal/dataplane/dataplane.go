package dataplane

import (
	"context"
	"errors"
	"fmt"
)

type ErrorCode string

const (
	CodeInvalid          ErrorCode = "invalid_argument"
	CodeNotFound         ErrorCode = "not_found"
	CodeConflict         ErrorCode = "conflict"
	CodePrecondition     ErrorCode = "precondition_failed"
	CodeUnauthorized     ErrorCode = "unauthorized"
	CodeForbidden        ErrorCode = "forbidden"
	CodeUnavailable      ErrorCode = "unavailable"
	CodeInternal         ErrorCode = "internal"
	CodeResourceExceeded ErrorCode = "resource_exceeded"
)

type Error struct {
	Code    ErrorCode      `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func IsCode(err error, code ErrorCode) bool {
	var target *Error
	return errors.As(err, &target) && target.Code == code
}

type DataPlane interface {
	Health(context.Context) (Health, error)
	ReadObject(context.Context, ReadRequest) (Object, error)
	WriteObject(context.Context, WriteRequest) (WriteResult, error)
	QueryObjects(context.Context, QueryRequest) (QueryResult, error)
	ExecuteBatch(context.Context, BatchRequest) (BatchResult, error)
	PlanSchema(context.Context, SchemaRequest) (SchemaResult, error)
	ApplySchema(context.Context, SchemaRequest) (SchemaResult, error)
	ReadChanges(context.Context, ChangesRequest) (ChangesResult, error)
	MaterializeEvent(context.Context, MaterializeRequest) (EventContext, error)
}
