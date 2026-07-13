package main

import (
	"path/filepath"
	"testing"
)

func TestMigrationCommandChecksAndApplies(t *testing.T) {
	if err := migrationCommand([]string{"--check", "--from", "1", "--to", "1"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BARQ_CONTROL_PATH", filepath.Join(t.TempDir(), "control.barq"))
	if err := migrationCommand([]string{"--apply", "--from", "1", "--to", "1"}); err != nil {
		t.Fatal(err)
	}
	if err := migrationCommand([]string{"--check", "--from", "1", "--to", "2"}); err == nil {
		t.Fatal("unsupported migration was accepted")
	}
}
