package deployment

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordingRunner struct {
	commands []string
	failOn   string
	failed   bool
}

func (runner *recordingRunner) Run(_ context.Context, _ string, _ io.Reader, stdout, _ io.Writer, name string, args ...string) error {
	command := name + " " + strings.Join(args, " ")
	runner.commands = append(runner.commands, command)
	if strings.Contains(command, "compose --env-file .env -f compose.yaml ps --format json") {
		_, _ = io.WriteString(stdout, `[{"Service":"core","State":"running","Health":"healthy"},{"Service":"control","State":"running","Health":"healthy"},{"Service":"edge","State":"running","Health":""}]`)
	}
	if name == "docker" && len(args) > 0 && args[0] == "run" && containsArg(args, "-czf") {
		for _, arg := range args {
			const prefix = "type=bind,src="
			const suffix = ",dst=/backup"
			if strings.HasPrefix(arg, prefix) && strings.HasSuffix(arg, suffix) {
				destination := strings.TrimSuffix(strings.TrimPrefix(arg, prefix), suffix)
				if err := writeTestArchive(filepath.Join(destination, "data.tar.gz"), "tenant/main.barq"); err != nil {
					return err
				}
			}
		}
	}
	if runner.failOn != "" && !runner.failed && strings.Contains(command, runner.failOn) {
		runner.failed = true
		return context.DeadlineExceeded
	}
	return nil
}

func TestBackupCreatesChecksummedConsistentSnapshot(t *testing.T) {
	dir := initTestDeployment(t)
	destination := filepath.Join(t.TempDir(), "backup")
	runner := &recordingRunner{}
	result, err := Backup(context.Background(), BackupOptions{
		Dir: dir, Destination: destination, Runner: runner,
		Now: func() time.Time { return time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != destination {
		t.Fatalf("backup path = %s", result.Path)
	}
	metadata, err := verifyBackup(destination)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Deployment.Project == "" || len(metadata.ConfigFiles) != len(configFiles) {
		t.Fatalf("bad metadata: %+v", metadata)
	}
	joined := strings.Join(runner.commands, "\n")
	for _, expected := range []string{"compose --env-file .env -f compose.yaml stop", "--entrypoint tar", "_barq-data", "compose --env-file .env -f compose.yaml up -d --wait"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("commands do not contain %q:\n%s", expected, joined)
		}
	}
}

func TestUpgradePinsReleaseAndKeepsRollbackState(t *testing.T) {
	dir := initTestDeployment(t)
	runner := &recordingRunner{}
	digestA := strings.Repeat("a", 64)
	digestB := strings.Repeat("b", 64)
	result, err := Upgrade(context.Background(), UpgradeOptions{
		Dir: dir, Version: "v2.0.0", Runner: runner,
		Now: func() time.Time { return time.Date(2026, 7, 13, 2, 0, 0, 0, time.UTC) },
		Resolve: func(version string) (Release, error) {
			return Release{Version: version, ControlImage: "control@sha256:" + digestA, CoreImage: "core@sha256:" + digestB}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.From != "v1" || result.To != "v2.0.0" || result.BackupPath == "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	manifest, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Release.Version != "v2.0.0" || len(manifest.Previous) != 1 || manifest.Previous[0].Version != "v1" {
		t.Fatalf("unexpected release history: %+v", manifest)
	}
	environment := readTestFile(t, filepath.Join(dir, ".env"))
	if !strings.Contains(environment, "BARQ_CORE_IMAGE=core@sha256:"+digestB) {
		t.Fatal("environment did not move to fixed Core digest")
	}
	if !strings.Contains(strings.Join(runner.commands, "\n"), "pull core control") {
		t.Fatal("upgrade did not pull both release images")
	}
}

func TestUpgradeRestoresOldStateWhenComposeFails(t *testing.T) {
	dir := initTestDeployment(t)
	beforeEnvironment := readTestFile(t, filepath.Join(dir, ".env"))
	runner := &recordingRunner{failOn: "pull core control"}
	digest := strings.Repeat("c", 64)
	_, err := Upgrade(context.Background(), UpgradeOptions{
		Dir: dir, Version: "v2", Runner: runner,
		Now: func() time.Time { return time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC) },
		Resolve: func(version string) (Release, error) {
			return Release{Version: version, ControlImage: "control@sha256:" + digest, CoreImage: "core@sha256:" + digest}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "was rolled back") {
		t.Fatalf("unexpected error: %v", err)
	}
	manifest, loadErr := LoadManifest(dir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if manifest.Version != "v1" || readTestFile(t, filepath.Join(dir, ".env")) != beforeEnvironment {
		t.Fatal("failed upgrade did not restore deployment state")
	}
}

func TestVerifyBackupRejectsArchiveTraversal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.tar.gz")
	if err := writeTestArchive(path, "../../outside"); err != nil {
		t.Fatal(err)
	}
	if err := validateTarArchive(path); err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDoctorReportsDeploymentHealth(t *testing.T) {
	dir := initTestDeployment(t)
	runner := &recordingRunner{}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{"status":"ok"}`))}, nil
	})}
	checks, err := Doctor(context.Background(), DoctorOptions{Dir: dir, Runner: runner, HTTPClient: client, MinimumBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !hasCheck(checks, "disk space", CheckPass) || !hasCheck(checks, "public health", CheckPass) || !hasCheck(checks, "backups", CheckWarn) {
		t.Fatalf("unexpected checks: %+v", checks)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func initTestDeployment(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "barq")
	_, err := Init(InitOptions{Dir: dir, Domain: "db.example.com", Version: "v1", ControlImage: "control:v1", CoreImage: "core:v1"})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeTestArchive(path, name string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	contents := []byte("barq")
	if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := archive.Write(contents); err != nil {
		return err
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if err := compressed.Close(); err != nil {
		return err
	}
	return file.Close()
}

func containsArg(args []string, wanted string) bool {
	for _, arg := range args {
		if arg == wanted {
			return true
		}
	}
	return false
}

func hasCheck(checks []Check, name string, status CheckStatus) bool {
	for _, check := range checks {
		if check.Name == name && check.Status == status {
			return true
		}
	}
	return false
}
