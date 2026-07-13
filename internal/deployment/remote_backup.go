package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const remoteBackupConfigName = "secrets/backup.json"
const remoteBackupStatusName = "remote-backup-status.json"

type RemoteBackupConfig struct {
	Repository   string `json:"repository"`
	Password     string `json:"password"`
	AccessKey    string `json:"access_key"`
	SecretKey    string `json:"secret_key"`
	SessionToken string `json:"session_token,omitempty"`
	Region       string `json:"region"`
	LocalKeep    int    `json:"local_keep"`
	DailyKeep    int    `json:"daily_keep"`
	WeeklyKeep   int    `json:"weekly_keep"`
	MonthlyKeep  int    `json:"monthly_keep"`
}

type ConfigureRemoteBackupOptions struct {
	Dir          string
	Repository   string
	Password     string
	AccessKey    string
	SecretKey    string
	SessionToken string
	Region       string
	Runner       Runner
	Stdout       io.Writer
	Stderr       io.Writer
}

type ConfigureRemoteBackupResult struct {
	RecoveryKeyPath string
}

func ConfigureRemoteBackup(ctx context.Context, options ConfigureRemoteBackupOptions) (ConfigureRemoteBackupResult, error) {
	dir, err := resolveDir(options.Dir)
	if err != nil {
		return ConfigureRemoteBackupResult{}, err
	}
	if _, err := LoadManifest(dir); err != nil {
		return ConfigureRemoteBackupResult{}, err
	}
	lock, err := acquireMaintenanceLock(dir)
	if err != nil {
		return ConfigureRemoteBackupResult{}, err
	}
	defer lock.release()
	repository := strings.TrimSpace(options.Repository)
	if !strings.HasPrefix(repository, "s3:") || strings.ContainsAny(repository, "\r\n") {
		return ConfigureRemoteBackupResult{}, errors.New("repository must be an S3-compatible restic URL starting with s3:")
	}
	if options.AccessKey == "" || options.SecretKey == "" {
		return ConfigureRemoteBackupResult{}, errors.New("S3 access key and secret key are required")
	}
	password := options.Password
	if password == "" {
		password, err = randomSecret(32)
		if err != nil {
			return ConfigureRemoteBackupResult{}, err
		}
	}
	config := RemoteBackupConfig{
		Repository: repository, Password: password, AccessKey: options.AccessKey, SecretKey: options.SecretKey,
		SessionToken: options.SessionToken, Region: valueOr(options.Region, "us-east-1"),
		LocalKeep: 3, DailyKeep: 7, WeeklyKeep: 4, MonthlyKeep: 12,
	}
	if err := validateRemoteConfig(config); err != nil {
		return ConfigureRemoteBackupResult{}, err
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return ConfigureRemoteBackupResult{}, err
	}
	configPath := filepath.Join(dir, filepath.FromSlash(remoteBackupConfigName))
	recoveryPath := filepath.Join(dir, "secrets", "backup-recovery-key.txt")
	oldConfig := saveExistingFile(configPath)
	oldRecovery := saveExistingFile(recoveryPath)
	if err := writeFile(dir, remoteBackupConfigName, append(encoded, '\n'), 0o600); err != nil {
		return ConfigureRemoteBackupResult{}, err
	}
	if err := os.WriteFile(recoveryPath, []byte(password+"\n"), 0o600); err != nil {
		_ = oldConfig.restore(configPath)
		_ = oldRecovery.restore(recoveryPath)
		return ConfigureRemoteBackupResult{}, err
	}
	if err := os.Chmod(recoveryPath, 0o600); err != nil {
		_ = oldConfig.restore(configPath)
		_ = oldRecovery.restore(recoveryPath)
		return ConfigureRemoteBackupResult{}, err
	}
	runner := defaultRunner(options.Runner)
	var ignored io.Writer = io.Discard
	if initErr := runRestic(ctx, runner, dir, config, ignored, ignored, "init"); initErr != nil {
		if verifyErr := runRestic(ctx, runner, dir, config, ignored, ignored, "cat", "config"); verifyErr != nil {
			configRestoreErr := oldConfig.restore(configPath)
			recoveryRestoreErr := oldRecovery.restore(recoveryPath)
			if configRestoreErr != nil || recoveryRestoreErr != nil {
				return ConfigureRemoteBackupResult{}, fmt.Errorf("open encrypted backup repository: %v; restore old configuration: %v %v", verifyErr, configRestoreErr, recoveryRestoreErr)
			}
			return ConfigureRemoteBackupResult{}, fmt.Errorf("open encrypted backup repository: %w", verifyErr)
		}
	}
	return ConfigureRemoteBackupResult{RecoveryKeyPath: recoveryPath}, nil
}

type RemoteBackupOptions struct {
	Dir    string
	Runner Runner
	Stdout io.Writer
	Stderr io.Writer
	Now    func() time.Time
}

type RemoteBackupResult struct {
	LocalPath string
}

type RemoteBackupStatus struct {
	LastBackupAt      *time.Time `json:"last_backup_at,omitempty"`
	LastCheckAt       *time.Time `json:"last_check_at,omitempty"`
	LastRestoreTestAt *time.Time `json:"last_restore_test_at,omitempty"`
	LastLocalPath     string     `json:"last_local_path,omitempty"`
}

func RemoteBackup(ctx context.Context, options RemoteBackupOptions) (RemoteBackupResult, error) {
	dir, err := resolveDir(options.Dir)
	if err != nil {
		return RemoteBackupResult{}, err
	}
	lock, err := acquireMaintenanceLock(dir)
	if err != nil {
		return RemoteBackupResult{}, err
	}
	defer lock.release()
	manifest, err := LoadManifest(dir)
	if err != nil {
		return RemoteBackupResult{}, err
	}
	config, err := loadRemoteBackupConfig(dir)
	if err != nil {
		return RemoteBackupResult{}, err
	}
	runner := defaultRunner(options.Runner)
	stdout, stderr := defaultWriter(options.Stdout), defaultWriter(options.Stderr)
	backup, err := Backup(ctx, BackupOptions{Dir: dir, Runner: runner, Stdout: stdout, Stderr: stderr, Now: options.Now, skipLock: true})
	if err != nil {
		return RemoteBackupResult{}, err
	}
	result := RemoteBackupResult{LocalPath: backup.Path}
	if err := runRestic(ctx, runner, dir, config, stdout, stderr, "backup", backup.Path, "--host", manifest.Project, "--tag", "barq", "--tag", "project="+manifest.Project); err != nil {
		return result, fmt.Errorf("upload encrypted backup; local backup remains at %s: %w", backup.Path, err)
	}
	forgetArgs := []string{
		"forget", "--host", manifest.Project,
		"--keep-daily", fmt.Sprint(config.DailyKeep),
		"--keep-weekly", fmt.Sprint(config.WeeklyKeep),
		"--keep-monthly", fmt.Sprint(config.MonthlyKeep), "--prune",
	}
	if err := runRestic(ctx, runner, dir, config, stdout, stderr, forgetArgs...); err != nil {
		return result, fmt.Errorf("backup uploaded but retention cleanup failed: %w", err)
	}
	completedAt := currentTime(options.Now)
	status := loadRemoteBackupStatus(dir)
	status.LastBackupAt, status.LastLocalPath = &completedAt, backup.Path
	if err := writeRemoteBackupStatus(dir, status); err != nil {
		return result, fmt.Errorf("backup uploaded but local status was not saved: %w", err)
	}
	pruneLocalBackups(filepath.Join(dir, "backups"), config.LocalKeep)
	return result, nil
}

type RemoteCheckOptions struct {
	Dir         string
	Runner      Runner
	Stdout      io.Writer
	Stderr      io.Writer
	RestoreTest bool
}

func CheckRemoteBackup(ctx context.Context, options RemoteCheckOptions) error {
	dir, err := resolveDir(options.Dir)
	if err != nil {
		return err
	}
	lock, err := acquireMaintenanceLock(dir)
	if err != nil {
		return err
	}
	defer lock.release()
	manifest, err := LoadManifest(dir)
	if err != nil {
		return err
	}
	config, err := loadRemoteBackupConfig(dir)
	if err != nil {
		return err
	}
	runner := defaultRunner(options.Runner)
	stdout, stderr := defaultWriter(options.Stdout), defaultWriter(options.Stderr)
	if err := runRestic(ctx, runner, dir, config, stdout, stderr, "check", "--read-data-subset=5%"); err != nil {
		return fmt.Errorf("check encrypted backup repository: %w", err)
	}
	checkedAt := time.Now().UTC()
	status := loadRemoteBackupStatus(dir)
	status.LastCheckAt = &checkedAt
	if !options.RestoreTest {
		return writeRemoteBackupStatus(dir, status)
	}
	target, err := os.MkdirTemp(dir, ".restore-test-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(target)
	if err := runRestic(ctx, runner, dir, config, stdout, stderr, "restore", "latest", "--host", manifest.Project, "--target", target); err != nil {
		return fmt.Errorf("restore latest backup for testing: %w", err)
	}
	if _, err := findVerifiedBackup(target, manifest.Project); err != nil {
		return fmt.Errorf("verify restored backup: %w", err)
	}
	status.LastRestoreTestAt = &checkedAt
	return writeRemoteBackupStatus(dir, status)
}

type RemoteRestoreOptions struct {
	Dir      string
	Snapshot string
	Runner   Runner
	Stdout   io.Writer
	Stderr   io.Writer
	Now      func() time.Time
}

func RestoreRemoteBackup(ctx context.Context, options RemoteRestoreOptions) (RestoreResult, error) {
	dir, err := resolveDir(options.Dir)
	if err != nil {
		return RestoreResult{}, err
	}
	lock, err := acquireMaintenanceLock(dir)
	if err != nil {
		return RestoreResult{}, err
	}
	defer lock.release()
	manifest, err := LoadManifest(dir)
	if err != nil {
		return RestoreResult{}, err
	}
	config, err := loadRemoteBackupConfig(dir)
	if err != nil {
		return RestoreResult{}, err
	}
	snapshot := strings.TrimSpace(options.Snapshot)
	if snapshot == "" {
		snapshot = "latest"
	}
	if !regexp.MustCompile(`^(latest|[0-9a-f]{4,64})$`).MatchString(snapshot) {
		return RestoreResult{}, errors.New("snapshot must be latest or a restic snapshot ID")
	}
	target, err := os.MkdirTemp(dir, ".remote-restore-")
	if err != nil {
		return RestoreResult{}, err
	}
	defer os.RemoveAll(target)
	runner := defaultRunner(options.Runner)
	stdout, stderr := defaultWriter(options.Stdout), defaultWriter(options.Stderr)
	if err := runRestic(ctx, runner, dir, config, stdout, stderr, "restore", snapshot, "--host", manifest.Project, "--target", target); err != nil {
		return RestoreResult{}, fmt.Errorf("download encrypted backup: %w", err)
	}
	backupPath, err := findVerifiedBackup(target, manifest.Project)
	if err != nil {
		return RestoreResult{}, err
	}
	return Restore(ctx, RestoreOptions{
		Dir: dir, Backup: backupPath, Runner: runner, Stdout: stdout, Stderr: stderr, SafetyBackup: true, Now: options.Now, skipLock: true,
	})
}

type existingFile struct {
	data   []byte
	mode   os.FileMode
	exists bool
}

func saveExistingFile(path string) existingFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return existingFile{}
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return existingFile{data: data, mode: mode, exists: true}
}

func (file existingFile) restore(path string) error {
	if !file.exists {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.WriteFile(path, file.data, file.mode); err != nil {
		return err
	}
	return os.Chmod(path, file.mode)
}

func loadRemoteBackupConfig(dir string) (RemoteBackupConfig, error) {
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(remoteBackupConfigName)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RemoteBackupConfig{}, errors.New("remote backup is not configured; run barqctl backup configure first")
		}
		return RemoteBackupConfig{}, err
	}
	var config RemoteBackupConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return RemoteBackupConfig{}, fmt.Errorf("read remote backup configuration: %w", err)
	}
	if err := validateRemoteConfig(config); err != nil {
		return RemoteBackupConfig{}, err
	}
	return config, nil
}

func loadRemoteBackupStatus(dir string) RemoteBackupStatus {
	data, err := os.ReadFile(filepath.Join(dir, remoteBackupStatusName))
	if err != nil {
		return RemoteBackupStatus{}
	}
	var status RemoteBackupStatus
	if json.Unmarshal(data, &status) != nil {
		return RemoteBackupStatus{}
	}
	return status
}

func writeRemoteBackupStatus(dir string, status RemoteBackupStatus) error {
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(dir, remoteBackupStatusName, append(data, '\n'), 0o644)
}

func validateRemoteConfig(config RemoteBackupConfig) error {
	for name, value := range map[string]string{
		"repository": config.Repository, "password": config.Password, "access key": config.AccessKey,
		"secret key": config.SecretKey, "session token": config.SessionToken, "region": config.Region,
	} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("%s contains invalid characters", name)
		}
	}
	if !strings.HasPrefix(config.Repository, "s3:") || config.Password == "" || config.AccessKey == "" || config.SecretKey == "" || config.Region == "" {
		return errors.New("remote backup configuration is incomplete")
	}
	if config.LocalKeep < 1 || config.DailyKeep < 1 || config.WeeklyKeep < 1 || config.MonthlyKeep < 1 {
		return errors.New("backup retention values must be positive")
	}
	return nil
}

func runRestic(ctx context.Context, runner Runner, dir string, config RemoteBackupConfig, stdout, stderr io.Writer, args ...string) error {
	environment := []string{
		"RESTIC_REPOSITORY=" + config.Repository,
		"RESTIC_PASSWORD=" + config.Password,
		"AWS_ACCESS_KEY_ID=" + config.AccessKey,
		"AWS_SECRET_ACCESS_KEY=" + config.SecretKey,
		"AWS_DEFAULT_REGION=" + config.Region,
	}
	if config.SessionToken != "" {
		environment = append(environment, "AWS_SESSION_TOKEN="+config.SessionToken)
	}
	return runner.Run(ctx, dir, nil, stdout, stderr, environment, resticExecutable(), args...)
}

func resticExecutable() string {
	executable, err := os.Executable()
	if err == nil {
		name := "restic"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		bundled := filepath.Join(filepath.Dir(executable), name)
		if info, err := os.Stat(bundled); err == nil && !info.IsDir() {
			return bundled
		}
	}
	return "restic"
}

func findVerifiedBackup(root, project string) (string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "backup.json" {
			return nil
		}
		dir := filepath.Dir(path)
		metadata, err := verifyBackup(dir)
		if err == nil && metadata.Deployment.Project == project {
			found = append(found, dir)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(found) == 0 {
		return "", errors.New("no valid backup for this deployment was restored")
	}
	sort.Strings(found)
	return found[len(found)-1], nil
}

func pruneLocalBackups(root string, keep int) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var verified []string
	for _, entry := range entries {
		if entry.IsDir() {
			path := filepath.Join(root, entry.Name())
			if _, err := verifyBackup(path); err == nil {
				verified = append(verified, path)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(verified)))
	for _, path := range verified[minimum(keep, len(verified)):] {
		_ = os.RemoveAll(path)
	}
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func minimum(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func currentTime(now func() time.Time) time.Time {
	if now != nil {
		return now().UTC()
	}
	return time.Now().UTC()
}
