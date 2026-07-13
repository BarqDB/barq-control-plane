package deployment

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInitCreatesPrivateRunnableBundle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "barq")
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	digestA := strings.Repeat("a", 64)
	digestB := strings.Repeat("b", 64)
	resolver := func(version string) (Release, error) {
		return Release{Version: version, ControlImage: "ghcr.io/barqdb/barq-control-plane@sha256:" + digestA, CoreImage: "ghcr.io/barqdb/barq-core@sha256:" + digestB}, nil
	}
	result, err := Init(InitOptions{Dir: dir, Domain: "DB.Example.com", Version: "v1.2.3", Now: func() time.Time { return now }, Resolve: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://db.example.com" || len(result.APIKey) < 40 {
		t.Fatalf("unexpected init result: %+v", result)
	}

	environment := readTestFile(t, filepath.Join(dir, ".env"))
	for _, expected := range []string{
		"BARQ_PROJECT=barq-db-",
		"BARQ_DOMAIN=db.example.com",
		"BARQ_CONTROL_IMAGE=ghcr.io/barqdb/barq-control-plane@sha256:" + digestA,
		"BARQ_CORE_IMAGE=ghcr.io/barqdb/barq-core@sha256:" + digestB,
		"BARQ_API_KEY=" + result.APIKey,
	} {
		if !strings.Contains(environment, expected) {
			t.Errorf(".env does not contain %q", expected)
		}
	}
	if mode := fileMode(t, filepath.Join(dir, ".env")); mode != 0o600 {
		t.Fatalf(".env mode = %o", mode)
	}
	if mode := fileMode(t, filepath.Join(dir, "secrets", "jwt-private.pem")); mode != 0o600 {
		t.Fatalf("private key mode = %o", mode)
	}
	privateBlock, _ := pem.Decode([]byte(readTestFile(t, filepath.Join(dir, "secrets", "jwt-private.pem"))))
	if privateBlock == nil {
		t.Fatal("private key is not PEM")
	}
	if _, err := x509.ParsePKCS8PrivateKey(privateBlock.Bytes); err != nil {
		t.Fatalf("private key: %v", err)
	}
	publicBlock, _ := pem.Decode([]byte(readTestFile(t, filepath.Join(dir, "secrets", "jwt-public.pem"))))
	if publicBlock == nil {
		t.Fatal("public key is not PEM")
	}
	if _, err := x509.ParsePKIXPublicKey(publicBlock.Bytes); err != nil {
		t.Fatalf("public key: %v", err)
	}

	manifestData := readTestFile(t, filepath.Join(dir, manifestName))
	var manifest Manifest
	if err := json.Unmarshal([]byte(manifestData), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Domain != "db.example.com" || manifest.Project == "" || manifest.Release.Version != "v1.2.3" ||
		manifest.Release.BundleVersion != CurrentBundleVersion || !manifest.CreatedAt.Equal(now) {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	compose := readTestFile(t, filepath.Join(dir, "compose.yaml"))
	if !strings.Contains(compose, "internal: true") || !strings.Contains(compose, "barq-data:/var/lib/barq") ||
		!strings.Contains(compose, "caddy:2.10.2-alpine@sha256:") {
		t.Fatal("compose bundle is missing private networking or shared storage")
	}

	if _, err := Init(InitOptions{Dir: dir, Domain: "db.example.com", Version: "v1.2.3", Resolve: resolver}); err == nil {
		t.Fatal("second init should not overwrite deployment secrets")
	}
}

func TestInitRejectsUnsafeDomain(t *testing.T) {
	for _, domain := range []string{"", "https://db.example.com", "db.example.com/path", "bad_name.example.com", "-bad.example.com"} {
		if _, err := Init(InitOptions{Dir: filepath.Join(t.TempDir(), "barq"), Domain: domain, Version: "v1", ControlImage: "control:test", CoreImage: "core:test"}); err == nil {
			t.Errorf("domain %q should fail", domain)
		}
	}
}

func TestInitRejectsMutableReleaseImages(t *testing.T) {
	resolver := func(version string) (Release, error) {
		return Release{Version: version, ControlImage: "control:" + version, CoreImage: "core:" + version}, nil
	}
	if _, err := Init(InitOptions{Dir: filepath.Join(t.TempDir(), "barq"), Domain: "db.example.com", Version: "v1", Resolve: resolver}); err == nil || !strings.Contains(err.Error(), "fixed sha256") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInitRejectsUnsignedReleaseBeforeWritingSecrets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "barq")
	digest := strings.Repeat("a", 64)
	_, err := Init(InitOptions{
		Dir: dir, Domain: "db.example.com", Version: "v1",
		Resolve: func(version string) (Release, error) {
			return Release{Version: version, ControlImage: "control@sha256:" + digest, CoreImage: "core@sha256:" + digest}, nil
		},
		Verify: func(Release) error { return errors.New("signature not found") },
	})
	if err == nil || !strings.Contains(err.Error(), "signature not found") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed verification wrote deployment files: %v", statErr)
	}
}

func TestLoadManifestNeedsInitializedDeployment(t *testing.T) {
	if _, err := LoadManifest(t.TempDir()); err == nil || !strings.Contains(err.Error(), "run barqctl init") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
