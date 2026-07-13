package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
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

	for _, check := range checks {
		if check.Status == CheckFail {
			return checks, fmt.Errorf("doctor found one or more failed checks")
		}
	}
	return checks, nil
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
