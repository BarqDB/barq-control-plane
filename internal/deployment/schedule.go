package deployment

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

type BackupScheduleOptions struct {
	Dir     string
	DailyAt string
	Runner  Runner
	Stdout  io.Writer
	Stderr  io.Writer
	Binary  string
	UnitDir string
}

type BackupScheduleResult struct {
	DailyTimer string
	CheckTimer string
}

func InstallBackupSchedule(ctx context.Context, options BackupScheduleOptions) (BackupScheduleResult, error) {
	dir, err := resolveDir(options.Dir)
	if err != nil {
		return BackupScheduleResult{}, err
	}
	manifest, err := LoadManifest(dir)
	if err != nil {
		return BackupScheduleResult{}, err
	}
	lock, err := acquireMaintenanceLock(dir)
	if err != nil {
		return BackupScheduleResult{}, err
	}
	defer lock.release()
	if _, err := loadRemoteBackupConfig(dir); err != nil {
		return BackupScheduleResult{}, err
	}
	dailyAt := options.DailyAt
	if dailyAt == "" {
		dailyAt = "03:00"
	}
	parsed, err := time.Parse("15:04", dailyAt)
	if err != nil {
		return BackupScheduleResult{}, errors.New("daily time must use 24-hour HH:MM format")
	}
	binary := options.Binary
	if binary == "" {
		binary, err = os.Executable()
		if err != nil {
			return BackupScheduleResult{}, err
		}
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return BackupScheduleResult{}, err
	}
	unitDir := options.UnitDir
	if unitDir == "" {
		unitDir = defaultUnitDir
	}
	return installBackupSchedule(ctx, schedulePlatformOptions{
		Dir: dir, Project: manifest.Project, DailyAt: parsed, Binary: binary, UnitDir: unitDir,
		Runner: defaultRunner(options.Runner), Stdout: defaultWriter(options.Stdout), Stderr: defaultWriter(options.Stderr),
	})
}

type schedulePlatformOptions struct {
	Dir     string
	Project string
	DailyAt time.Time
	Binary  string
	UnitDir string
	Runner  Runner
	Stdout  io.Writer
	Stderr  io.Writer
}
