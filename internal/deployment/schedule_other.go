//go:build !linux

package deployment

import (
	"context"
	"errors"
)

func installBackupSchedule(context.Context, schedulePlatformOptions) (BackupScheduleResult, error) {
	return BackupScheduleResult{}, errors.New("automatic backup schedules currently require a Linux server with systemd")
}
