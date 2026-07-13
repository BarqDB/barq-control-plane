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
	commands      []string
	environments  [][]string
	failOn        string
	failAlways    string
	failAt        int
	matchCount    int
	failed        bool
	restoreBackup string
}

func (runner *recordingRunner) Run(_ context.Context, _ string, _ io.Reader, stdout, _ io.Writer, environment []string, name string, args ...string) error {
	command := name + " " + strings.Join(args, " ")
	runner.commands = append(runner.commands, command)
	runner.environments = append(runner.environments, append([]string(nil), environment...))
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
	if filepath.Base(name) == "restic" || filepath.Base(name) == "restic.exe" {
		if len(args) > 0 && args[0] == "restore" && runner.restoreBackup != "" {
			for index, arg := range args {
				if arg == "--target" && index+1 < len(args) {
					target := filepath.Join(args[index+1], "restored", filepath.Base(runner.restoreBackup))
					if err := os.MkdirAll(target, 0o700); err != nil {
						return err
					}
					if err := os.CopyFS(target, os.DirFS(runner.restoreBackup)); err != nil {
						return err
					}
				}
			}
		}
	}
	if runner.failOn != "" && !runner.failed && strings.Contains(command, runner.failOn) {
		runner.matchCount++
		failAt := runner.failAt
		if failAt == 0 {
			failAt = 1
		}
		if runner.matchCount == failAt {
			runner.failed = true
			return context.DeadlineExceeded
		}
	}
	if runner.failAlways != "" && strings.Contains(command, runner.failAlways) {
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

func TestMaintenanceLockRejectsOverlappingWork(t *testing.T) {
	dir := initTestDeployment(t)
	lock, err := acquireMaintenanceLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.release()
	_, err = Backup(context.Background(), BackupOptions{Dir: dir, Destination: filepath.Join(t.TempDir(), "backup"), Runner: &recordingRunner{}})
	if err == nil || !strings.Contains(err.Error(), "another Barq maintenance command") {
		t.Fatalf("unexpected lock error: %v", err)
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
	commands := strings.Join(runner.commands, "\n")
	if !strings.Contains(commands, "docker pull core@sha256:") || !strings.Contains(commands, "docker pull control@sha256:") ||
		!strings.Contains(commands, "migrate --check --from 1 --to 1") || !strings.Contains(commands, "migrate --apply --from 1 --to 1") {
		t.Fatalf("upgrade did not pull and migrate the release:\n%s", commands)
	}
}

func TestUpgradeRestoresOldStateWhenComposeFails(t *testing.T) {
	dir := initTestDeployment(t)
	beforeEnvironment := readTestFile(t, filepath.Join(dir, ".env"))
	runner := &recordingRunner{failOn: "run --rm --no-deps --entrypoint /usr/local/bin/barq-server control migrate --apply"}
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

func TestUpgradeRejectsIncompatibleReleaseBeforeDockerOrDowntime(t *testing.T) {
	dir := initTestDeployment(t)
	runner := &recordingRunner{}
	digest := strings.Repeat("d", 64)
	_, err := Upgrade(context.Background(), UpgradeOptions{
		Dir: dir, Version: "v3", Runner: runner,
		Resolve: func(version string) (Release, error) {
			return Release{
				Version: version, ControlImage: "control@sha256:" + digest, CoreImage: "core@sha256:" + digest,
				InternalProtocol: 1, CoreDataFormat: 2, ControlSchema: 1,
			}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "Core data format") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("incompatible release changed Docker state: %v", runner.commands)
	}
}

func TestUpgradeRestoresSnapshotWhenNewReleaseIsUnhealthy(t *testing.T) {
	dir := initTestDeployment(t)
	beforeEnvironment := readTestFile(t, filepath.Join(dir, ".env"))
	runner := &recordingRunner{failOn: "up -d --wait", failAt: 2}
	digest := strings.Repeat("e", 64)
	_, err := Upgrade(context.Background(), UpgradeOptions{
		Dir: dir, Version: "v2", Runner: runner,
		Now: func() time.Time { return time.Date(2026, 7, 13, 4, 0, 0, 0, time.UTC) },
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
		t.Fatal("health failure did not restore the old release")
	}
	commands := strings.Join(runner.commands, "\n")
	if !strings.Contains(commands, "find /target") {
		t.Fatalf("health failure did not restore the data snapshot:\n%s", commands)
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
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"status":"ok"}`
		if request.URL.Path == "/v1/operations/health" {
			if request.Header.Get("Authorization") == "" {
				t.Fatal("operations health did not use the deployment API key")
			}
			body = `{"status":"ok","pending":0,"retrying":0,"dead_transform":0,"dead_delivery":0}`
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	checks, err := Doctor(context.Background(), DoctorOptions{Dir: dir, Runner: runner, HTTPClient: client, MinimumBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !hasCheck(checks, "disk space", CheckPass) || !hasCheck(checks, "public health", CheckPass) || !hasCheck(checks, "webhook queues", CheckPass) || !hasCheck(checks, "dead letters", CheckPass) || !hasCheck(checks, "backups", CheckWarn) {
		t.Fatalf("unexpected checks: %+v", checks)
	}
}

func TestEncryptedRemoteBackupAndRestoreCheck(t *testing.T) {
	dir := initTestDeployment(t)
	runner := &recordingRunner{}
	configured, err := ConfigureRemoteBackup(context.Background(), ConfigureRemoteBackupOptions{
		Dir: dir, Repository: "s3:https://s3.example.com/backups/client-a", Region: "eu-west-1",
		AccessKey: "access", SecretKey: "secret", Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if configured.RecoveryKeyPath == "" {
		t.Fatal("recovery key path is empty")
	}
	for _, path := range []string{filepath.Join(dir, filepath.FromSlash(remoteBackupConfigName)), configured.RecoveryKeyPath} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("secret file mode for %s: %v %v", path, info, err)
		}
	}
	result, err := RemoteBackup(context.Background(), RemoteBackupOptions{
		Dir: dir, Runner: runner, Now: func() time.Time { return time.Date(2026, 7, 13, 5, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LocalPath == "" {
		t.Fatal("remote backup did not keep a local copy")
	}
	joined := strings.Join(runner.commands, "\n")
	if !strings.Contains(joined, "restic backup") || !strings.Contains(joined, "restic forget") {
		t.Fatalf("remote commands missing:\n%s", joined)
	}
	environments := strings.Join(flatten(runner.environments), "\n")
	if !strings.Contains(environments, "RESTIC_PASSWORD=") || !strings.Contains(environments, "AWS_SECRET_ACCESS_KEY=secret") {
		t.Fatalf("restic environment is incomplete: %v", runner.environments)
	}
	if strings.Contains(joined, "secret") {
		t.Fatal("S3 secret leaked into command arguments")
	}
	runner.restoreBackup = result.LocalPath
	if err := CheckRemoteBackup(context.Background(), RemoteCheckOptions{Dir: dir, Runner: runner, RestoreTest: true}); err != nil {
		t.Fatal(err)
	}
	status := loadRemoteBackupStatus(dir)
	if status.LastBackupAt == nil || status.LastCheckAt == nil || status.LastRestoreTestAt == nil {
		t.Fatalf("remote status was not recorded: %+v", status)
	}
}

func TestRemoteBackupReconfigureFailureKeepsWorkingCredentials(t *testing.T) {
	dir := initTestDeployment(t)
	if _, err := ConfigureRemoteBackup(context.Background(), ConfigureRemoteBackupOptions{
		Dir: dir, Repository: "s3:https://s3.example.com/backups/working",
		AccessKey: "access", SecretKey: "working-secret", Runner: &recordingRunner{},
	}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, filepath.FromSlash(remoteBackupConfigName))
	before := readTestFile(t, configPath)
	_, err := ConfigureRemoteBackup(context.Background(), ConfigureRemoteBackupOptions{
		Dir: dir, Repository: "s3:https://s3.example.com/backups/broken",
		AccessKey: "bad", SecretKey: "bad", Runner: &recordingRunner{failAlways: "restic"},
	})
	if err == nil {
		t.Fatal("broken repository configuration was accepted")
	}
	if after := readTestFile(t, configPath); after != before {
		t.Fatal("failed reconfiguration replaced working credentials")
	}
}

func TestS3EncryptedBackupIntegration(t *testing.T) {
	repository := os.Getenv("BARQ_TEST_S3_REPOSITORY")
	if repository == "" {
		t.Skip("BARQ_TEST_S3_REPOSITORY is not set")
	}
	dir := initTestDeployment(t)
	if _, err := ConfigureRemoteBackup(context.Background(), ConfigureRemoteBackupOptions{
		Dir: dir, Repository: repository, Region: "us-east-1",
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		Password: "barq-ci-restic-password", Runner: ExecRunner{},
	}); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "barq-backup")
	if _, err := Backup(context.Background(), BackupOptions{
		Dir: dir, Destination: local, Runner: &recordingRunner{},
		Now: func() time.Time { return time.Date(2026, 7, 13, 6, 0, 0, 0, time.UTC) },
	}); err != nil {
		t.Fatal(err)
	}
	config, err := loadRemoteBackupConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := runRestic(context.Background(), ExecRunner{}, dir, config, io.Discard, io.Discard,
		"backup", local, "--host", manifest.Project, "--tag", "barq-ci"); err != nil {
		t.Fatal(err)
	}
	if err := CheckRemoteBackup(context.Background(), RemoteCheckOptions{Dir: dir, Runner: ExecRunner{}, RestoreTest: true}); err != nil {
		t.Fatal(err)
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

func flatten(values [][]string) []string {
	var result []string
	for _, value := range values {
		result = append(result, value...)
	}
	return result
}
