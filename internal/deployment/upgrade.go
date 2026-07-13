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
)

type UpgradeOptions struct {
	Dir     string
	Version string
	Runner  Runner
	Stdout  io.Writer
	Stderr  io.Writer
	Resolve func(string) (Release, error)
	Now     func() time.Time
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
	if release.Version != version || !fixedImage(release.ControlImage) || !fixedImage(release.CoreImage) {
		return UpgradeResult{}, errors.New("target release is invalid or does not use fixed image digests")
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
	manifest.Previous = manifest.Previous[:len(manifest.Previous)-1]
	return changeRelease(ctx, dir, manifest, target, options.Runner, options.Stdout, options.Stderr, options.Now)
}

func changeRelease(ctx context.Context, dir string, manifest Manifest, target Release, runner Runner, stdout, stderr io.Writer, now func() time.Time) (UpgradeResult, error) {
	runner = defaultRunner(runner)
	stdout, stderr = defaultWriter(stdout), defaultWriter(stderr)
	current := manifest.Release
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
		_ = writeFile(dir, ".env", oldEnvironment, 0o600)
		return UpgradeResult{}, err
	}
	applyErr := runCompose(ctx, runner, dir, nil, stdout, stderr, "pull", "core", "control")
	if applyErr == nil {
		applyErr = runCompose(ctx, runner, dir, nil, stdout, stderr, "up", "-d", "--wait")
	}
	if applyErr != nil {
		_ = writeFile(dir, ".env", oldEnvironment, 0o600)
		_ = writeFile(dir, manifestName, oldManifest, 0o644)
		rollbackErr := runCompose(context.Background(), runner, dir, nil, stdout, stderr, "up", "-d", "--wait")
		if rollbackErr != nil {
			return UpgradeResult{}, fmt.Errorf("release failed: %v; automatic rollback also failed: %v; safety backup: %s", applyErr, rollbackErr, backup.Path)
		}
		return UpgradeResult{}, fmt.Errorf("release failed and was rolled back: %w; safety backup: %s", applyErr, backup.Path)
	}
	return UpgradeResult{From: current.Version, To: target.Version, BackupPath: backup.Path}, nil
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
