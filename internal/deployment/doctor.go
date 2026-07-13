package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type CheckStatus string

const (
	CheckPass CheckStatus = "pass"
	CheckWarn CheckStatus = "warn"
	CheckFail CheckStatus = "fail"
)

type Check struct {
	Name   string
	Status CheckStatus
	Detail string
}

type DoctorOptions struct {
	Dir          string
	Runner       Runner
	HTTPClient   *http.Client
	Stdout       io.Writer
	Stderr       io.Writer
	MinimumBytes uint64
	Now          func() time.Time
}

func Doctor(ctx context.Context, options DoctorOptions) ([]Check, error) {
	dir, err := resolveDir(options.Dir)
	if err != nil {
		return nil, err
	}
	manifest, err := LoadManifest(dir)
	if err != nil {
		return []Check{{Name: "deployment", Status: CheckFail, Detail: err.Error()}}, err
	}
	checks := []Check{{Name: "deployment", Status: CheckPass, Detail: manifest.Version + " in " + dir}}
	for _, name := range []string{".env", "compose.yaml", "Caddyfile", "secrets/jwt-private.pem", "secrets/jwt-public.pem"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			checks = append(checks, Check{Name: name, Status: CheckFail, Detail: err.Error()})
		} else {
			checks = append(checks, Check{Name: name, Status: CheckPass, Detail: "present"})
		}
	}
	for _, name := range []string{".env", "secrets/jwt-private.pem"} {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
		if err == nil && info.Mode().Perm()&0o077 != 0 {
			checks = append(checks, Check{Name: name + " permissions", Status: CheckFail, Detail: fmt.Sprintf("mode %o exposes secrets", info.Mode().Perm())})
		} else if err == nil {
			checks = append(checks, Check{Name: name + " permissions", Status: CheckPass, Detail: fmt.Sprintf("mode %o", info.Mode().Perm())})
		}
	}
	if fixedImage(manifest.Release.ControlImage) && fixedImage(manifest.Release.CoreImage) {
		checks = append(checks, Check{Name: "image pins", Status: CheckPass, Detail: "both images use SHA-256 digests"})
	} else {
		checks = append(checks, Check{Name: "image pins", Status: CheckWarn, Detail: "development images are mutable"})
	}
	minimum := options.MinimumBytes
	if minimum == 0 {
		minimum = 5 << 30
	}
	if free, err := diskFreeBytes(dir); err != nil {
		checks = append(checks, Check{Name: "disk space", Status: CheckWarn, Detail: err.Error()})
	} else if free < minimum {
		checks = append(checks, Check{Name: "disk space", Status: CheckFail, Detail: fmt.Sprintf("%s free; at least %s recommended", byteSize(free), byteSize(minimum))})
	} else {
		checks = append(checks, Check{Name: "disk space", Status: CheckPass, Detail: byteSize(free) + " free"})
	}

	runner := defaultRunner(options.Runner)
	stdout, stderr := defaultWriter(options.Stdout), defaultWriter(options.Stderr)
	if err := runCompose(ctx, runner, dir, nil, stdout, stderr, "config", "--quiet"); err != nil {
		checks = append(checks, Check{Name: "Docker Compose", Status: CheckFail, Detail: err.Error()})
	} else {
		checks = append(checks, Check{Name: "Docker Compose", Status: CheckPass, Detail: "configuration is valid"})
	}
	var services bytes.Buffer
	if err := runCompose(ctx, runner, dir, nil, &services, stderr, "ps", "--format", "json"); err != nil {
		checks = append(checks, Check{Name: "containers", Status: CheckFail, Detail: err.Error()})
	} else if detail, err := healthyServices(services.Bytes()); err != nil {
		checks = append(checks, Check{Name: "containers", Status: CheckFail, Detail: err.Error()})
	} else {
		checks = append(checks, Check{Name: "containers", Status: CheckPass, Detail: detail})
	}

	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, manifest.URL+"/health/ready", nil)
	if err == nil {
		response, requestErr := client.Do(request)
		if requestErr != nil {
			checks = append(checks, Check{Name: "public health", Status: CheckFail, Detail: requestErr.Error()})
		} else {
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				checks = append(checks, Check{Name: "public health", Status: CheckPass, Detail: response.Status})
			} else {
				checks = append(checks, Check{Name: "public health", Status: CheckFail, Detail: response.Status})
			}
		}
	}
	checks = append(checks, webhookHealthCheck(ctx, client, dir, manifest.URL, now)...)

	backupTime, backupPath := newestBackup(filepath.Join(dir, "backups"))
	if backupTime.IsZero() {
		checks = append(checks, Check{Name: "backups", Status: CheckWarn, Detail: "no verified local backup found"})
	} else {
		age := now().Sub(backupTime)
		status := CheckPass
		if age > 24*time.Hour {
			status = CheckWarn
		}
		checks = append(checks, Check{Name: "backups", Status: status, Detail: fmt.Sprintf("latest is %s old: %s", age.Round(time.Minute), backupPath)})
	}
	checks = append(checks, remoteBackupChecks(dir, now())...)

	for _, check := range checks {
		if check.Status == CheckFail {
			return checks, fmt.Errorf("doctor found one or more failed checks")
		}
	}
	return checks, nil
}

func remoteBackupChecks(dir string, now time.Time) []Check {
	configPath := filepath.Join(dir, filepath.FromSlash(remoteBackupConfigName))
	info, err := os.Stat(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return []Check{{Name: "remote backups", Status: CheckWarn, Detail: "encrypted S3 backup is not configured"}}
	}
	if err != nil {
		return []Check{{Name: "remote backups", Status: CheckFail, Detail: err.Error()}}
	}
	checks := make([]Check, 0, 3)
	if info.Mode().Perm()&0o077 != 0 {
		checks = append(checks, Check{Name: "backup credentials", Status: CheckFail, Detail: fmt.Sprintf("mode %o exposes secrets", info.Mode().Perm())})
	} else if _, err := loadRemoteBackupConfig(dir); err != nil {
		checks = append(checks, Check{Name: "backup credentials", Status: CheckFail, Detail: err.Error()})
	} else {
		checks = append(checks, Check{Name: "backup credentials", Status: CheckPass, Detail: fmt.Sprintf("mode %o", info.Mode().Perm())})
	}
	status := loadRemoteBackupStatus(dir)
	checks = append(checks, ageCheck("remote backups", status.LastBackupAt, now, 26*time.Hour, "no successful encrypted upload recorded"))
	checks = append(checks, ageCheck("restore tests", status.LastRestoreTestAt, now, 8*24*time.Hour, "no successful full restore test recorded"))
	return checks
}

func ageCheck(name string, completed *time.Time, now time.Time, maximum time.Duration, missing string) Check {
	if completed == nil {
		return Check{Name: name, Status: CheckWarn, Detail: missing}
	}
	age := now.Sub(*completed)
	if age < 0 {
		age = 0
	}
	status := CheckPass
	if age > maximum {
		status = CheckWarn
	}
	return Check{Name: name, Status: status, Detail: fmt.Sprintf("last success was %s ago", age.Round(time.Minute))}
}

type webhookHealth struct {
	Pending         int        `json:"pending"`
	Retrying        int        `json:"retrying"`
	DeadTransform   int        `json:"dead_transform"`
	DeadDelivery    int        `json:"dead_delivery"`
	OldestPendingAt *time.Time `json:"oldest_pending_at"`
}

func webhookHealthCheck(ctx context.Context, client *http.Client, dir, baseURL string, now func() time.Time) []Check {
	apiKey, err := environmentValue(filepath.Join(dir, ".env"), "BARQ_API_KEY")
	if err != nil {
		return []Check{{Name: "webhook queues", Status: CheckWarn, Detail: err.Error()}}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/operations/health", nil)
	if err != nil {
		return []Check{{Name: "webhook queues", Status: CheckWarn, Detail: err.Error()}}
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response, err := client.Do(request)
	if err != nil {
		return []Check{{Name: "webhook queues", Status: CheckWarn, Detail: err.Error()}}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return []Check{{Name: "webhook queues", Status: CheckWarn, Detail: response.Status}}
	}
	var health webhookHealth
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&health); err != nil {
		return []Check{{Name: "webhook queues", Status: CheckWarn, Detail: "invalid server response: " + err.Error()}}
	}
	checks := make([]Check, 0, 2)
	queueStatus := CheckPass
	queueDetail := fmt.Sprintf("%d pending, %d retrying", health.Pending, health.Retrying)
	if health.OldestPendingAt != nil && now().Sub(*health.OldestPendingAt) > 5*time.Minute {
		queueStatus = CheckWarn
		queueDetail += fmt.Sprintf("; oldest is %s old", now().Sub(*health.OldestPendingAt).Round(time.Minute))
	}
	checks = append(checks, Check{Name: "webhook queues", Status: queueStatus, Detail: queueDetail})
	deadStatus := CheckPass
	if health.DeadTransform+health.DeadDelivery > 0 {
		deadStatus = CheckWarn
	}
	checks = append(checks, Check{Name: "dead letters", Status: deadStatus, Detail: fmt.Sprintf("%d transform, %d delivery", health.DeadTransform, health.DeadDelivery)})
	return checks
}

func environmentValue(path, wanted string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found && key == wanted && value != "" {
			return value, nil
		}
	}
	return "", fmt.Errorf("%s is missing", wanted)
}

func healthyServices(data []byte) (string, error) {
	var services []struct {
		Service string `json:"Service"`
		State   string `json:"State"`
		Health  string `json:"Health"`
	}
	if err := json.Unmarshal(data, &services); err != nil {
		return "", fmt.Errorf("read container state: %w", err)
	}
	wanted := map[string]bool{"core": false, "control": false, "edge": false}
	for _, service := range services {
		if _, exists := wanted[service.Service]; !exists {
			continue
		}
		if service.State != "running" {
			return "", fmt.Errorf("%s is %s", service.Service, service.State)
		}
		if (service.Service == "core" || service.Service == "control") && service.Health != "healthy" {
			return "", fmt.Errorf("%s health is %q", service.Service, service.Health)
		}
		wanted[service.Service] = true
	}
	for service, found := range wanted {
		if !found {
			return "", fmt.Errorf("%s is not running", service)
		}
	}
	return "core, control, and edge are running", nil
}

func newestBackup(root string) (time.Time, string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return time.Time{}, ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name())
		metadata, err := verifyBackup(path)
		if err == nil {
			return metadata.CreatedAt, path
		}
	}
	return time.Time{}, ""
}

func byteSize(bytes uint64) string {
	const gib = uint64(1 << 30)
	if bytes >= gib {
		return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(gib))
	}
	return fmt.Sprintf("%.1f MiB", float64(bytes)/float64(1<<20))
}
