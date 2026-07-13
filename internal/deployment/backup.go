package deployment

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const backupFormat = 1

var configFiles = []string{
	".env",
	"compose.yaml",
	"Caddyfile",
	manifestName,
	"secrets/jwt-private.pem",
	"secrets/jwt-public.pem",
}

type BackupOptions struct {
	Dir         string
	Destination string
	Runner      Runner
	Stdout      io.Writer
	Stderr      io.Writer
	Now         func() time.Time
	skipLock    bool
}

type BackupResult struct {
	Path string
}

type BackupMetadata struct {
	Format      int               `json:"format"`
	CreatedAt   time.Time         `json:"created_at"`
	Deployment  Manifest          `json:"deployment"`
	DataSHA256  string            `json:"data_sha256"`
	ConfigFiles map[string]string `json:"config_files"`
}

func Backup(ctx context.Context, options BackupOptions) (result BackupResult, resultErr error) {
	dir, err := resolveDir(options.Dir)
	if err != nil {
		return result, err
	}
	manifest, err := LoadManifest(dir)
	if err != nil {
		return result, err
	}
	if manifest.Project == "" || manifest.Release.CoreImage == "" {
		return result, errors.New("deployment manifest is missing project or release data; run barqctl init with a current release")
	}
	if !options.skipLock {
		lock, err := acquireMaintenanceLock(dir)
		if err != nil {
			return result, err
		}
		defer lock.release()
		manifest, err = LoadManifest(dir)
		if err != nil {
			return result, err
		}
	}
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	destination := options.Destination
	if destination == "" {
		destination = filepath.Join(dir, "backups", now().UTC().Format("20060102T150405Z"))
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return result, err
	}
	if _, err := os.Stat(destination); err == nil {
		return result, fmt.Errorf("backup destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if err := os.MkdirAll(filepath.Join(destination, "config", "secrets"), 0o700); err != nil {
		return result, err
	}
	configHashes := make(map[string]string, len(configFiles))
	for _, name := range configFiles {
		source := filepath.Join(dir, filepath.FromSlash(name))
		target := filepath.Join(destination, "config", filepath.FromSlash(name))
		mode := os.FileMode(0o600)
		if name == "compose.yaml" || name == "Caddyfile" || name == manifestName || name == "secrets/jwt-public.pem" {
			mode = 0o644
		}
		if err := copyFile(source, target, mode); err != nil {
			return result, err
		}
		hash, err := hashFile(target)
		if err != nil {
			return result, err
		}
		configHashes[name] = hash
	}

	runner := defaultRunner(options.Runner)
	stdout, stderr := defaultWriter(options.Stdout), defaultWriter(options.Stderr)
	if err := runCompose(ctx, runner, dir, nil, stdout, stderr, "stop", "edge", "control", "core"); err != nil {
		return result, fmt.Errorf("stop Barq for backup: %w", err)
	}
	defer func() {
		if restartErr := runCompose(context.Background(), runner, dir, nil, stdout, stderr, "up", "-d", "--wait"); restartErr != nil {
			if resultErr == nil {
				resultErr = fmt.Errorf("backup finished but Barq did not restart: %w", restartErr)
			}
		}
	}()

	dataArchive := filepath.Join(destination, "data.tar.gz")
	file, err := os.OpenFile(dataArchive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return result, err
	}
	if err := file.Close(); err != nil {
		return result, err
	}
	volume := manifest.Project + "_barq-data"
	dockerArgs := []string{
		"run", "--rm", "--user", "0", "--entrypoint", "tar",
		"--mount", "type=volume,src=" + volume + ",dst=/source,readonly",
		"--mount", "type=bind,src=" + destination + ",dst=/backup",
		manifest.Release.CoreImage,
		"-czf", "/backup/data.tar.gz", "-C", "/source", ".",
	}
	if err := runner.Run(ctx, dir, nil, stdout, stderr, nil, "docker", dockerArgs...); err != nil {
		return result, fmt.Errorf("archive Barq data: %w", err)
	}
	dataHash, err := hashFile(dataArchive)
	if err != nil {
		return result, err
	}
	metadata := BackupMetadata{
		Format: backupFormat, CreatedAt: now().UTC(), Deployment: manifest,
		DataSHA256: dataHash, ConfigFiles: configHashes,
	}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return result, err
	}
	encoded = append(encoded, '\n')
	if err := writeFile(destination, "backup.json", encoded, 0o600); err != nil {
		return result, err
	}
	return BackupResult{Path: destination}, nil
}

type RestoreOptions struct {
	Dir          string
	Backup       string
	Runner       Runner
	Stdout       io.Writer
	Stderr       io.Writer
	SafetyBackup bool
	Now          func() time.Time
	skipLock     bool
}

type RestoreResult struct {
	SafetyBackup string
}

func Restore(ctx context.Context, options RestoreOptions) (result RestoreResult, resultErr error) {
	dir, err := resolveDir(options.Dir)
	if err != nil {
		return result, err
	}
	active, err := LoadManifest(dir)
	if err != nil {
		return result, err
	}
	if !options.skipLock {
		lock, err := acquireMaintenanceLock(dir)
		if err != nil {
			return result, err
		}
		defer lock.release()
		active, err = LoadManifest(dir)
		if err != nil {
			return result, err
		}
	}
	backupPath, err := filepath.Abs(options.Backup)
	if err != nil {
		return result, err
	}
	metadata, err := verifyBackup(backupPath)
	if err != nil {
		return result, err
	}
	if metadata.Deployment.Project != active.Project {
		return result, fmt.Errorf("backup belongs to project %q, not %q", metadata.Deployment.Project, active.Project)
	}
	runner := defaultRunner(options.Runner)
	stdout, stderr := defaultWriter(options.Stdout), defaultWriter(options.Stderr)
	if options.SafetyBackup {
		safety, err := Backup(ctx, BackupOptions{Dir: dir, Runner: runner, Stdout: stdout, Stderr: stderr, Now: options.Now, skipLock: true})
		if err != nil {
			return result, fmt.Errorf("create safety backup: %w", err)
		}
		result.SafetyBackup = safety.Path
	}
	if err := runCompose(ctx, runner, dir, nil, stdout, stderr, "down"); err != nil {
		return result, fmt.Errorf("stop Barq for restore: %w", err)
	}
	defer func() {
		if restartErr := runCompose(context.Background(), runner, dir, nil, stdout, stderr, "up", "-d", "--wait"); restartErr != nil && resultErr == nil {
			resultErr = fmt.Errorf("restore finished but Barq did not restart: %w", restartErr)
		}
	}()

	volume := active.Project + "_barq-data"
	image := active.Release.CoreImage
	if image == "" {
		image = metadata.Deployment.Release.CoreImage
	}
	dockerArgs := []string{
		"run", "--rm", "--user", "0", "--entrypoint", "sh",
		"--mount", "type=volume,src=" + volume + ",dst=/target",
		"--mount", "type=bind,src=" + backupPath + ",dst=/backup,readonly",
		image, "-ec", "find /target -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +; tar -xzf /backup/data.tar.gz -C /target",
	}
	if err := runner.Run(ctx, dir, nil, stdout, stderr, nil, "docker", dockerArgs...); err != nil {
		return result, fmt.Errorf("restore Barq data: %w", err)
	}
	for _, name := range configFiles {
		source := filepath.Join(backupPath, "config", filepath.FromSlash(name))
		target := filepath.Join(dir, filepath.FromSlash(name))
		mode := os.FileMode(0o600)
		if name == "compose.yaml" || name == "Caddyfile" || name == manifestName || name == "secrets/jwt-public.pem" {
			mode = 0o644
		}
		if err := copyFile(source, target, mode); err != nil {
			return result, err
		}
	}
	return result, nil
}

func verifyBackup(path string) (BackupMetadata, error) {
	data, err := os.ReadFile(filepath.Join(path, "backup.json"))
	if err != nil {
		return BackupMetadata{}, fmt.Errorf("read backup metadata: %w", err)
	}
	var metadata BackupMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return BackupMetadata{}, fmt.Errorf("decode backup metadata: %w", err)
	}
	if metadata.Format != backupFormat {
		return BackupMetadata{}, fmt.Errorf("unsupported backup format %d", metadata.Format)
	}
	dataHash, err := hashFile(filepath.Join(path, "data.tar.gz"))
	if err != nil {
		return BackupMetadata{}, err
	}
	if dataHash != metadata.DataSHA256 {
		return BackupMetadata{}, errors.New("backup data checksum does not match")
	}
	if err := validateTarArchive(filepath.Join(path, "data.tar.gz")); err != nil {
		return BackupMetadata{}, err
	}
	if len(metadata.ConfigFiles) != len(configFiles) {
		return BackupMetadata{}, errors.New("backup does not contain the full deployment configuration")
	}
	for _, name := range configFiles {
		expected, ok := metadata.ConfigFiles[name]
		if !ok {
			return BackupMetadata{}, fmt.Errorf("backup is missing %s", name)
		}
		actual, err := hashFile(filepath.Join(path, "config", filepath.FromSlash(name)))
		if err != nil {
			return BackupMetadata{}, err
		}
		if actual != expected {
			return BackupMetadata{}, fmt.Errorf("backup checksum does not match for %s", name)
		}
	}
	return metadata, nil
}

func validateTarArchive(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open backup data archive: %w", err)
	}
	defer compressed.Close()
	archive := tar.NewReader(compressed)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read backup data archive: %w", err)
		}
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe path in backup data archive: %s", header.Name)
		}
		if header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink {
			return fmt.Errorf("links are not allowed in backup data archive: %s", header.Name)
		}
	}
}

func copyFile(source, target string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open %s: %w", source, err)
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Chmod(target, mode)
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
