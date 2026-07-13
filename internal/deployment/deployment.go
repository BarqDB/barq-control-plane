package deployment

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	deployfiles "github.com/barqdb/barq-server/deploy"
)

const manifestName = "deployment.json"

type Manifest struct {
	Version   string    `json:"version"`
	Domain    string    `json:"domain"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
}

type InitOptions struct {
	Dir          string
	Domain       string
	Version      string
	ControlImage string
	CoreImage    string
	Force        bool
	Now          func() time.Time
	Resolve      func(string) (Release, error)
}

type InitResult struct {
	Dir    string
	URL    string
	APIKey string
}

type Release struct {
	Version      string `json:"version"`
	ControlImage string `json:"control_image"`
	CoreImage    string `json:"core_image"`
}

func DefaultDir() (string, error) {
	if value := os.Getenv("BARQ_HOME"); value != "" {
		return filepath.Abs(value)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".barq"), nil
}

func Init(options InitOptions) (InitResult, error) {
	dir, err := resolveDir(options.Dir)
	if err != nil {
		return InitResult{}, err
	}
	domain, err := cleanDomain(options.Domain)
	if err != nil {
		return InitResult{}, err
	}
	version := strings.TrimSpace(options.Version)
	if version == "" {
		return InitResult{}, errors.New("release version is required")
	}
	if (options.ControlImage == "") != (options.CoreImage == "") {
		return InitResult{}, errors.New("control-image and core-image must be provided together")
	}
	if options.ControlImage == "" {
		resolve := ResolveRelease
		if options.Resolve != nil {
			resolve = options.Resolve
		}
		release, err := resolve(version)
		if err != nil {
			return InitResult{}, fmt.Errorf("resolve release %s: %w", version, err)
		}
		if release.Version != version {
			return InitResult{}, fmt.Errorf("release manifest version is %q, expected %q", release.Version, version)
		}
		if !fixedImage(release.ControlImage) || !fixedImage(release.CoreImage) {
			return InitResult{}, errors.New("release manifest images must use fixed sha256 digests")
		}
		options.ControlImage = release.ControlImage
		options.CoreImage = release.CoreImage
	}
	if err := prepareDirectory(dir, options.Force); err != nil {
		return InitResult{}, err
	}

	internalSecret, err := randomSecret(32)
	if err != nil {
		return InitResult{}, err
	}
	apiKey, err := randomSecret(32)
	if err != nil {
		return InitResult{}, err
	}
	privatePEM, publicPEM, err := jwtKeyPair()
	if err != nil {
		return InitResult{}, err
	}
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	manifest := Manifest{Version: version, Domain: domain, URL: "https://" + domain, CreatedAt: now().UTC()}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return InitResult{}, err
	}
	manifestJSON = append(manifestJSON, '\n')
	environment := fmt.Sprintf("BARQ_DOMAIN=%s\nBARQ_CONTROL_IMAGE=%s\nBARQ_CORE_IMAGE=%s\nBARQ_INTERNAL_SECRET=%s\nBARQ_API_KEY=%s\nBARQ_TENANT=default\nBARQ_DATABASE=default\nBARQ_LOG_LEVEL=info\nBARQ_ALLOW_PRIVATE_WEBHOOKS=false\n",
		domain, options.ControlImage, options.CoreImage, internalSecret, apiKey)

	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"compose.yaml", deployfiles.Compose, 0o644},
		{"Caddyfile", deployfiles.Caddyfile, 0o644},
		{manifestName, manifestJSON, 0o644},
		{".env", []byte(environment), 0o600},
		{filepath.Join("secrets", "jwt-private.pem"), privatePEM, 0o600},
		{filepath.Join("secrets", "jwt-public.pem"), publicPEM, 0o644},
	}
	for _, file := range files {
		if err := writeFile(dir, file.name, file.data, file.mode); err != nil {
			return InitResult{}, err
		}
	}
	return InitResult{Dir: dir, URL: manifest.URL, APIKey: apiKey}, nil
}

func ResolveRelease(version string) (Release, error) {
	if version == "" || strings.ContainsAny(version, "/\\?# ") {
		return Release{}, errors.New("invalid release version")
	}
	endpoint := "https://github.com/BarqDB/barq-control-plane/releases/download/" + url.PathEscape(version) + "/release.json"
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Get(endpoint)
	if err != nil {
		return Release{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("download release manifest: %s", response.Status)
	}
	var release Release
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&release); err != nil {
		return Release{}, fmt.Errorf("decode release manifest: %w", err)
	}
	return release, nil
}

func Compose(ctx context.Context, dir string, stdout, stderr io.Writer, args ...string) error {
	dir, err := resolveDir(dir)
	if err != nil {
		return err
	}
	if _, err := LoadManifest(dir); err != nil {
		return err
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("Docker is required; install Docker Engine or Docker Desktop first")
	}
	commandArgs := []string{"compose", "--env-file", ".env", "-f", "compose.yaml"}
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(ctx, "docker", commandArgs...)
	command.Dir = dir
	command.Stdout = stdout
	command.Stderr = stderr
	command.Stdin = os.Stdin
	if err := command.Run(); err != nil {
		return fmt.Errorf("docker compose: %w", err)
	}
	return nil
}

func LoadManifest(dir string) (Manifest, error) {
	dir, err := resolveDir(dir)
	if err != nil {
		return Manifest{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Manifest{}, fmt.Errorf("no Barq deployment in %s; run barqctl init first", dir)
		}
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("read deployment manifest: %w", err)
	}
	return manifest, nil
}

func Open(dir string) (string, error) {
	manifest, err := LoadManifest(dir)
	if err != nil {
		return "", err
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", manifest.URL)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", manifest.URL)
	default:
		command = exec.Command("xdg-open", manifest.URL)
	}
	if err := command.Run(); err != nil {
		return manifest.URL, fmt.Errorf("open browser: %w", err)
	}
	return manifest.URL, nil
}

func resolveDir(dir string) (string, error) {
	if dir == "" {
		return DefaultDir()
	}
	resolved, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve deployment directory: %w", err)
	}
	return resolved, nil
}

func cleanDomain(value string) (string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "", errors.New("domain is required")
	}
	if strings.Contains(value, "://") || strings.ContainsAny(value, "/?#@ ") {
		return "", errors.New("domain must be a hostname without a scheme, path, or port")
	}
	parsed, err := url.Parse("https://" + value)
	if err != nil || parsed.Hostname() != value {
		return "", errors.New("domain must be a valid hostname or IP address")
	}
	if net.ParseIP(value) == nil {
		labels := strings.Split(value, ".")
		for _, label := range labels {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", errors.New("domain must be a valid hostname or IP address")
			}
			for _, character := range label {
				if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
					return "", errors.New("domain must be a valid hostname or IP address")
				}
			}
		}
	}
	return value, nil
}

func prepareDirectory(dir string, force bool) error {
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("deployment path is not a directory: %s", dir)
		}
		if !force {
			if _, err := os.Stat(filepath.Join(dir, manifestName)); err == nil {
				return fmt.Errorf("Barq is already initialized in %s", dir)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			if len(entries) != 0 {
				return fmt.Errorf("deployment directory is not empty: %s (use --force to keep unrelated files)", dir)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.MkdirAll(dir, 0o700)
}

func randomSecret(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func fixedImage(image string) bool {
	name, digest, found := strings.Cut(image, "@sha256:")
	if !found || name == "" || len(digest) != 64 {
		return false
	}
	for _, character := range digest {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func jwtKeyPair() ([]byte, []byte, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, nil, fmt.Errorf("generate JWT key: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	return privatePEM, publicPEM, nil
}

func writeFile(root, name string, data []byte, mode os.FileMode) error {
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return os.Chmod(path, mode)
}
