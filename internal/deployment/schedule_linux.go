//go:build linux

package deployment

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func installBackupSchedule(ctx context.Context, options schedulePlatformOptions) (BackupScheduleResult, error) {
	unitDir := filepath.Join(options.ConfigDir, "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		return BackupScheduleResult{}, err
	}
	base := "barq-backup-" + options.Project
	dailyService := base + ".service"
	dailyTimer := base + ".timer"
	checkService := base + "-check.service"
	checkTimer := base + "-check.timer"
	dailyCommand := strings.Join([]string{
		systemdQuote(options.Binary), "backup", "--remote", "--dir", systemdQuote(options.Dir),
	}, " ")
	checkCommand := strings.Join([]string{
		systemdQuote(options.Binary), "backup", "check", "--restore-test", "--dir", systemdQuote(options.Dir),
	}, " ")
	service := func(description, command string) []byte {
		return []byte(fmt.Sprintf("[Unit]\nDescription=%s\nAfter=network-online.target\n\n[Service]\nType=oneshot\nExecStart=%s\nNice=10\nIOSchedulingClass=best-effort\n", description, command))
	}
	dailyClock := options.DailyAt.Format("15:04")
	checkHour := (options.DailyAt.Hour() + 1) % 24
	checkClock := fmt.Sprintf("%02d:%02d", checkHour, options.DailyAt.Minute())
	timer := func(description, calendar string) []byte {
		return []byte(fmt.Sprintf("[Unit]\nDescription=%s\n\n[Timer]\nOnCalendar=%s\nPersistent=true\nRandomizedDelaySec=15m\n\n[Install]\nWantedBy=timers.target\n", description, calendar))
	}
	files := map[string][]byte{
		dailyService: service("Barq encrypted backup", dailyCommand),
		dailyTimer:   timer("Run Barq encrypted backup every day", "*-*-* "+dailyClock+":00"),
		checkService: service("Barq full backup restore test", checkCommand),
		checkTimer:   timer("Test a Barq backup restore every week", "Sun *-*-* "+checkClock+":00"),
	}
	for name, data := range files {
		if err := writeFile(unitDir, name, data, 0o644); err != nil {
			return BackupScheduleResult{}, err
		}
	}
	if err := options.Runner.Run(ctx, options.Dir, nil, options.Stdout, options.Stderr, nil, "systemctl", "--user", "daemon-reload"); err != nil {
		return BackupScheduleResult{}, fmt.Errorf("load backup schedule: %w", err)
	}
	if err := options.Runner.Run(ctx, options.Dir, nil, options.Stdout, options.Stderr, nil, "systemctl", "--user", "enable", "--now", dailyTimer, checkTimer); err != nil {
		return BackupScheduleResult{}, fmt.Errorf("enable backup schedule: %w", err)
	}
	return BackupScheduleResult{DailyTimer: dailyTimer, CheckTimer: checkTimer}, nil
}

func systemdQuote(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return "\"" + value + "\""
}
