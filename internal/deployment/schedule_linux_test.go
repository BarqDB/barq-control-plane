//go:build linux

package deployment

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallBackupScheduleCreatesDailyAndRestoreTimers(t *testing.T) {
	dir := initTestDeployment(t)
	runner := &recordingRunner{}
	if _, err := ConfigureRemoteBackup(context.Background(), ConfigureRemoteBackupOptions{
		Dir: dir, Repository: "s3:https://s3.example.com/backups/client-a",
		AccessKey: "access", SecretKey: "secret", Runner: runner,
	}); err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	result, err := InstallBackupSchedule(context.Background(), BackupScheduleOptions{
		Dir: dir, DailyAt: "02:30", Binary: "/opt/barq/bin/barqctl", ConfigDir: configDir, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.DailyTimer == "" || result.CheckTimer == "" {
		t.Fatalf("missing timer names: %+v", result)
	}
	unitDir := filepath.Join(configDir, "systemd", "user")
	daily, err := os.ReadFile(filepath.Join(unitDir, result.DailyTimer))
	if err != nil {
		t.Fatal(err)
	}
	check, err := os.ReadFile(filepath.Join(unitDir, result.CheckTimer))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(daily), "OnCalendar=*-*-* 02:30:00") || !strings.Contains(string(check), "OnCalendar=Sun *-*-* 03:30:00") {
		t.Fatalf("unexpected timers:\n%s\n%s", daily, check)
	}
	for _, entry := range []string{result.DailyTimer, result.CheckTimer} {
		info, err := os.Stat(filepath.Join(unitDir, entry))
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("unit mode: %v %v", info, err)
		}
	}
	joined := strings.Join(runner.commands, "\n")
	if !strings.Contains(joined, "systemctl --user daemon-reload") || !strings.Contains(joined, "systemctl --user enable --now") {
		t.Fatalf("systemd was not enabled:\n%s", joined)
	}
	services, _ := filepath.Glob(filepath.Join(unitDir, "*.service"))
	for _, service := range services {
		data, err := os.ReadFile(service)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "secret") || strings.Contains(string(data), "RESTIC_PASSWORD") {
			t.Fatalf("secret leaked into %s", service)
		}
	}
}
