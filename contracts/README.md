# Barq private data-plane contract

The Go gateway and the future C++ data process communicate through
`/internal/v1`. This protocol is private but versioned. Both implementations
must run the JSON fixtures in `fixtures/`.

## Rules

- `tenant` and `database` are required on all customer-data calls.
- System control state is not part of this protocol. The Go process opens its
  local `control.barq` directly through `github.com/BarqDB/barq-go`.
- Each write request is one transaction. A batch is one all-or-nothing
  transaction with at most 100 operations.
- `etag` is an opaque quoted token. Patch and delete require `if_match`.
- A stale `if_match` returns `conflict` and makes no change.
- Change cursors are unsigned, increasing values scoped to one tenant and
  logical database. A caller may receive the same event more than once.
- Event IDs are stable across retries and process restarts.
- `snapshot` identifies the exact committed snapshot used by event
  materialization. Update events have `before` and `after`; deletes use the
  before snapshot for related reads.
- Requests may carry `request_id`; writes may carry `idempotency_key`.
- Unknown JSON fields are rejected.

## Schema manifests

Schema changes in v1 are additive. `objects[].name` maps to the Barq
`class_<name>` table. `primary_key` and each item in `properties` have `name`,
`type`, and optional `nullable` fields. Supported types are `string`, `int`,
`bool`, `double`, `float`, `mixed`, `object_id`, and `uuid`.

Existing primary keys and field types cannot change. A new field on an object
type that already has rows must be nullable.

## Canonical JSON values

Normal JSON strings, booleans, nulls, arrays, objects, and safe numbers are
used directly. Barq values that JSON cannot safely represent use tagged values:

```json
{"$int64":"9223372036854775807"}
{"$decimal128":"12.50"}
{"$object_id":"64b7abdecf2160b649ab6085"}
{"$uuid":"5f43a3b0-f4f7-4fd3-9516-b2af49ee30af"}
{"$timestamp":"2026-07-12T08:30:00.123456789Z"}
{"$binary":"AAEC"}
```

Object fields beginning with `$` are reserved. Canonical serialization sorts
object keys by their UTF-8 name and does not add whitespace. It is used for
ETags, idempotency hashes, and fixture comparisons.

## Errors

Errors have this shape:

```json
{"code":"not_found","message":"object was not found","details":{}}
```

Codes are `invalid_argument`, `not_found`, `conflict`,
`precondition_failed`, `unauthorized`, `forbidden`, `resource_exceeded`,
`unavailable`, and `internal`.
