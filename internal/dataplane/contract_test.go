package dataplane

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSharedContractFixtures(t *testing.T) {
	var write WriteRequest
	readFixture(t, "write.request.json", &write)
	if !write.Scope.Valid() || write.Operation != WriteCreate || write.Type == "" || write.IdempotencyKey == "" {
		t.Fatalf("invalid write fixture: %+v", write)
	}
	var changes ChangesResult
	readFixture(t, "changes.response.json", &changes)
	if len(changes.Events) != 1 || changes.NextCursor != changes.Events[0].Cursor || changes.Events[0].Scope != write.Scope {
		t.Fatalf("invalid changes fixture: %+v", changes)
	}

	var materialize MaterializeRequest
	readFixture(t, "materialize.request.json", &materialize)
	if !materialize.Scope.Valid() || len(materialize.Reads) != 1 {
		t.Fatalf("invalid materialization fixture: %+v", materialize)
	}

	var schema SchemaRequest
	readFixture(t, "schema.request.json", &schema)
	if !schema.Scope.Valid() || schema.Version != 1 || !json.Valid(schema.Manifest) {
		t.Fatalf("invalid schema fixture: %+v", schema)
	}

	var health Health
	readFixture(t, "health.response.json", &health)
	if health.Status != "ok" || len(health.Capabilities) == 0 {
		t.Fatalf("invalid health fixture: %+v", health)
	}
}

func readFixture(t *testing.T, name string, target any) {
	t.Helper()
	path := filepath.Join("..", "..", "contracts", "fixtures", name)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		t.Fatal(err)
	}
}
