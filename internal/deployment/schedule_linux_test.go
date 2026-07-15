//go:build linux

package deployment

import (
	"context"
	"os"
	"os/user"
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
	unitDir := t.TempDir()
	result, err := InstallBackupSchedule(context.Background(), BackupScheduleOptions{
		Dir: dir, DailyAt: "02:30", Binary: "/opt/barq/bin/barqctl", UnitDir: unitDir, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.DailyTimer == "" || result.CheckTimer == "" {
		t.Fatalf("missing timer names: %+v", result)
	}
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

// A user timer only runs while its owner has a session, so an unattended server
// silently stops backing up after a logout or reboot.
func TestInstallBackupScheduleUsesSystemTimers(t *testing.T) {
	dir := initTestDeployment(t)
	runner := &recordingRunner{}
	if _, err := ConfigureRemoteBackup(context.Background(), ConfigureRemoteBackupOptions{
		Dir: dir, Repository: "s3:https://s3.example.com/backups/client-a",
		AccessKey: "access", SecretKey: "secret", Runner: runner,
	}); err != nil {
		t.Fatal(err)
	}
	unitDir := t.TempDir()
	if _, err := InstallBackupSchedule(context.Background(), BackupScheduleOptions{
		Dir: dir, DailyAt: "02:30", Binary: "/opt/barq/bin/barqctl", UnitDir: unitDir, Runner: runner,
	}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.commands, "\n")
	if strings.Contains(joined, "--user") {
		t.Fatalf("schedule used session-bound user units:\n%s", joined)
	}
	if !strings.Contains(joined, "systemctl daemon-reload") || !strings.Contains(joined, "systemctl enable --now") {
		t.Fatalf("system timers were not enabled:\n%s", joined)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	services, _ := filepath.Glob(filepath.Join(unitDir, "*.service"))
	if len(services) == 0 {
		t.Fatal("no service units were written")
	}
	for _, service := range services {
		data, err := os.ReadFile(service)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "User="+current.Username+"\n") {
			t.Fatalf("%s does not run as the deployment owner %q:\n%s", service, current.Username, data)
		}
	}
}

func TestInstallBackupScheduleReportsMissingRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write any unit directory")
	}
	dir := initTestDeployment(t)
	runner := &recordingRunner{}
	if _, err := ConfigureRemoteBackup(context.Background(), ConfigureRemoteBackupOptions{
		Dir: dir, Repository: "s3:https://s3.example.com/backups/client-a",
		AccessKey: "access", SecretKey: "secret", Runner: runner,
	}); err != nil {
		t.Fatal(err)
	}
	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
	_, err := InstallBackupSchedule(context.Background(), BackupScheduleOptions{
		Dir: dir, DailyAt: "02:30", Binary: "/opt/barq/bin/barqctl",
		UnitDir: filepath.Join(readOnly, "system"), Runner: runner,
	})
	if err == nil {
		t.Fatal("expected an error when the unit directory is not writable")
	}
	if !strings.Contains(err.Error(), "as root") {
		t.Fatalf("error does not explain that root is needed: %v", err)
	}
}
