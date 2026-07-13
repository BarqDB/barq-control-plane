package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	deployfiles "github.com/barqdb/barq-server/deploy"
)

type UpgradeOptions struct {
	Dir     string
	Version string
	Runner  Runner
	Stdout  io.Writer
	Stderr  io.Writer
	Resolve func(string) (Release, error)
	Now     func() time.Time
	Verify  func(Release) error
}

type UpgradeResult struct {
	From       string
	To         string
	BackupPath string
}

func Upgrade(ctx context.Context, options UpgradeOptions) (UpgradeResult, error) {
	dir, err := resolveDir(options.Dir)
	if err != nil {
		return UpgradeResult{}, err
	}
	version := strings.TrimSpace(options.Version)
	if version == "" {
		return UpgradeResult{}, errors.New("target release is required")
	}
	resolve := ResolveRelease
	if options.Resolve != nil {
		resolve = options.Resolve
	}
	release, err := resolve(version)
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("resolve release %s: %w", version, err)
	}
	release = normalizeRelease(release)
	if release.Version != version || !fixedImage(release.ControlImage) || !fixedImage(release.CoreImage) {
		return UpgradeResult{}, errors.New("target release is invalid or does not use fixed image digests")
	}
	if err := validateReleaseCompatibility(release); err != nil {
		return UpgradeResult{}, err
	}
	if options.Verify != nil {
		if err := options.Verify(release); err != nil {
			return UpgradeResult{}, fmt.Errorf("verify release %s: %w", version, err)
		}
	}
	lock, err := acquireMaintenanceLock(dir)
	if err != nil {
		return UpgradeResult{}, err
	}
	defer lock.release()
	manifest, err := LoadManifest(dir)
	if err != nil {
		return UpgradeResult{}, err
	}
	if release == manifest.Release {
		return UpgradeResult{}, fmt.Errorf("Barq is already on %s", version)
	}
	return changeRelease(ctx, dir, manifest, release, options.Runner, options.Stdout, options.Stderr, options.Now)
}

type RollbackOptions struct {
	Dir    string
	Runner Runner
	Stdout io.Writer
	Stderr io.Writer
	Now    func() time.Time
}

func Rollback(ctx context.Context, options RollbackOptions) (UpgradeResult, error) {
	dir, err := resolveDir(options.Dir)
	if err != nil {
		return UpgradeResult{}, err
	}
	lock, err := acquireMaintenanceLock(dir)
	if err != nil {
		return UpgradeResult{}, err
	}
	defer lock.release()
	manifest, err := LoadManifest(dir)
	if err != nil {
		return UpgradeResult{}, err
	}
	if len(manifest.Previous) == 0 {
		return UpgradeResult{}, errors.New("there is no previous release to roll back to")
	}
	target := manifest.Previous[len(manifest.Previous)-1]
	if !fixedImage(target.ControlImage) || !fixedImage(target.CoreImage) {
		return UpgradeResult{}, errors.New("cannot safely roll back to mutable development images")
	}
	if err := validateReleaseCompatibility(target); err != nil {
		return UpgradeResult{}, err
	}
	manifest.Previous = manifest.Previous[:len(manifest.Previous)-1]
	return changeRelease(ctx, dir, manifest, target, options.Runner, options.Stdout, options.Stderr, options.Now)
}

func changeRelease(ctx context.Context, dir string, manifest Manifest, target Release, runner Runner, stdout, stderr io.Writer, now func() time.Time) (UpgradeResult, error) {
	runner = defaultRunner(runner)
	stdout, stderr = defaultWriter(stdout), defaultWriter(stderr)
	current := manifest.Release
	if err := pullRelease(ctx, runner, dir, stdout, stderr, target); err != nil {
		return UpgradeResult{}, fmt.Errorf("download target release: %w", err)
	}
	if err := checkReleaseMigration(ctx, runner, dir, stdout, stderr, current, target); err != nil {
		return UpgradeResult{}, fmt.Errorf("target release preflight: %w", err)
	}
	backup, err := Backup(ctx, BackupOptions{Dir: dir, Runner: runner, Stdout: stdout, Stderr: stderr, Now: now, skipLock: true})
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("pre-upgrade backup: %w", err)
	}
	oldEnvironment, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		return UpgradeResult{}, err
	}
	oldManifest, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return UpgradeResult{}, err
	}
	oldCompose, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		return UpgradeResult{}, err
	}
	oldCaddyfile, err := os.ReadFile(filepath.Join(dir, "Caddyfile"))
	if err != nil {
		return UpgradeResult{}, err
	}
	restoreOldFiles := func() {
		_ = writeFile(dir, ".env", oldEnvironment, 0o600)
		_ = writeFile(dir, manifestName, oldManifest, 0o644)
		_ = writeFile(dir, "compose.yaml", oldCompose, 0o644)
		_ = writeFile(dir, "Caddyfile", oldCaddyfile, 0o644)
	}
	if target != current {
		manifest.Previous = append(manifest.Previous, current)
		if len(manifest.Previous) > 10 {
			manifest.Previous = manifest.Previous[len(manifest.Previous)-10:]
		}
	}
	manifest.Version = target.Version
	manifest.Release = target
	newEnvironment, err := replaceEnvironment(oldEnvironment, map[string]string{
		"BARQ_CONTROL_IMAGE": target.ControlImage,
		"BARQ_CORE_IMAGE":    target.CoreImage,
	})
	if err != nil {
		return UpgradeResult{}, err
	}
	if err := writeFile(dir, ".env", newEnvironment, 0o600); err != nil {
		return UpgradeResult{}, err
	}
	if err := writeManifest(dir, manifest); err != nil {
		restoreOldFiles()
		return UpgradeResult{}, err
	}
	if err := writeFile(dir, "compose.yaml", deployfiles.Compose, 0o644); err != nil {
		restoreOldFiles()
		return UpgradeResult{}, err
	}
	if err := writeFile(dir, "Caddyfile", deployfiles.Caddyfile, 0o644); err != nil {
		restoreOldFiles()
		return UpgradeResult{}, err
	}
	applyErr := runCompose(ctx, runner, dir, nil, stdout, stderr, "stop", "edge", "control")
	if applyErr == nil {
		applyErr = runCompose(ctx, runner, dir, nil, stdout, stderr,
			"run", "--rm", "--no-deps", "--entrypoint", "/usr/local/bin/barq-server", "control",
			"migrate", "--apply", "--from", fmt.Sprint(current.ControlSchema), "--to", fmt.Sprint(target.ControlSchema))
	}
	if applyErr == nil {
		applyErr = runCompose(ctx, runner, dir, nil, stdout, stderr, "up", "-d", "--wait")
	}
	if applyErr != nil {
		rollbackErr := restoreFailedRelease(runner, dir, stdout, stderr, oldEnvironment, oldManifest, oldCompose, oldCaddyfile, backup.Path)
		if rollbackErr != nil {
			return UpgradeResult{}, fmt.Errorf("release failed: %v; automatic rollback also failed: %v; safety backup: %s", applyErr, rollbackErr, backup.Path)
		}
		return UpgradeResult{}, fmt.Errorf("release failed and was rolled back: %w; safety backup: %s", applyErr, backup.Path)
	}
	return UpgradeResult{From: current.Version, To: target.Version, BackupPath: backup.Path}, nil
}

func pullRelease(ctx context.Context, runner Runner, dir string, stdout, stderr io.Writer, release Release) error {
	for _, image := range []string{release.CoreImage, release.ControlImage} {
		if err := runner.Run(ctx, dir, nil, stdout, stderr, nil, "docker", "pull", image); err != nil {
			return err
		}
	}
	return nil
}

func checkReleaseMigration(ctx context.Context, runner Runner, dir string, stdout, stderr io.Writer, current, target Release) error {
	if current.InternalProtocol != target.InternalProtocol || current.CoreDataFormat != target.CoreDataFormat {
		return errors.New("release changes an unsupported protocol or Core data format")
	}
	return runner.Run(ctx, dir, nil, stdout, stderr, nil, "docker", "run", "--rm",
		"--entrypoint", "/usr/local/bin/barq-server", target.ControlImage,
		"migrate", "--check", "--from", fmt.Sprint(current.ControlSchema), "--to", fmt.Sprint(target.ControlSchema))
}

func restoreFailedRelease(runner Runner, dir string, stdout, stderr io.Writer, oldEnvironment, oldManifest, oldCompose, oldCaddyfile []byte, backupPath string) error {
	if err := writeFile(dir, ".env", oldEnvironment, 0o600); err != nil {
		return err
	}
	if err := writeFile(dir, manifestName, oldManifest, 0o644); err != nil {
		return err
	}
	if err := writeFile(dir, "compose.yaml", oldCompose, 0o644); err != nil {
		return err
	}
	if err := writeFile(dir, "Caddyfile", oldCaddyfile, 0o644); err != nil {
		return err
	}
	_, err := Restore(context.Background(), RestoreOptions{
		Dir: dir, Backup: backupPath, Runner: runner, Stdout: stdout, Stderr: stderr, SafetyBackup: false, skipLock: true,
	})
	return err
}

func replaceEnvironment(data []byte, replacements map[string]string) ([]byte, error) {
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	found := make(map[string]bool, len(replacements))
	for index, line := range lines {
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if value, exists := replacements[key]; exists {
			if strings.ContainsAny(value, "\r\n") {
				return nil, fmt.Errorf("invalid environment value for %s", key)
			}
			lines[index] = key + "=" + value
			found[key] = true
		}
	}
	for key, value := range replacements {
		if !found[key] {
			lines = append(lines, key+"="+value)
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func writeManifest(dir string, manifest Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(dir, manifestName, append(data, '\n'), 0o644)
}
