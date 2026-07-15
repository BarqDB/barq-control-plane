//go:build linux

package deployment

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
)

const defaultUnitDir = "/etc/systemd/system"

func installBackupSchedule(ctx context.Context, options schedulePlatformOptions) (BackupScheduleResult, error) {
	owner, err := deploymentOwner(options.Dir)
	if err != nil {
		return BackupScheduleResult{}, err
	}
	if err := os.MkdirAll(options.UnitDir, 0o755); err != nil {
		return BackupScheduleResult{}, scheduleNeedsRoot(err)
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
		return []byte(fmt.Sprintf("[Unit]\nDescription=%s\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=oneshot\nExecStart=%s\nUser=%s\nNice=10\nIOSchedulingClass=best-effort\n", description, command, owner))
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
		if err := writeFile(options.UnitDir, name, data, 0o644); err != nil {
			return BackupScheduleResult{}, scheduleNeedsRoot(err)
		}
	}
	if err := options.Runner.Run(ctx, options.Dir, nil, options.Stdout, options.Stderr, nil, "systemctl", "daemon-reload"); err != nil {
		return BackupScheduleResult{}, fmt.Errorf("load backup schedule: %w", err)
	}
	if err := options.Runner.Run(ctx, options.Dir, nil, options.Stdout, options.Stderr, nil, "systemctl", "enable", "--now", dailyTimer, checkTimer); err != nil {
		return BackupScheduleResult{}, fmt.Errorf("enable backup schedule: %w", err)
	}
	return BackupScheduleResult{DailyTimer: dailyTimer, CheckTimer: checkTimer}, nil
}

// deploymentOwner keeps the scheduled run under the account that owns the
// deployment, so unattended backups write the same files as a manual run.
func deploymentOwner(dir string) (string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("read deployment directory owner")
	}
	owner, err := user.LookupId(strconv.FormatUint(uint64(stat.Uid), 10))
	if err != nil {
		return "", fmt.Errorf("read deployment directory owner: %w", err)
	}
	return owner.Username, nil
}

func scheduleNeedsRoot(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w; a system timer keeps running after logout and across reboots, so run barqctl backup schedule as root", err)
	}
	return err
}

func systemdQuote(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return "\"" + value + "\""
}
