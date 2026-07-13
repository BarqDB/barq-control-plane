package deployment

import (
	"fmt"
	"os"
	"path/filepath"
)

type maintenanceLock struct {
	file *os.File
}

func acquireMaintenanceLock(dir string) (*maintenanceLock, error) {
	path := filepath.Join(dir, ".maintenance.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLockFile(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another Barq maintenance command is running: %w", err)
	}
	if err := file.Truncate(0); err == nil {
		_, _ = file.WriteString(fmt.Sprintf("pid=%d\n", os.Getpid()))
		_ = file.Sync()
	}
	return &maintenanceLock{file: file}, nil
}

func (lock *maintenanceLock) release() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = unlockFile(lock.file)
	_ = lock.file.Close()
}
