package deployment

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// SetAPIKey updates only the local operator credential used by barqctl health
// checks. It does not create, rotate, or revoke a server-side key.
func SetAPIKey(dir, raw string) error {
	dir, err := resolveDir(dir)
	if err != nil {
		return err
	}
	if _, err := LoadManifest(dir); err != nil {
		return err
	}
	raw = strings.TrimSpace(raw)
	if len(raw) < 8 || len(raw) > 512 || strings.ContainsAny(raw, "\r\n\t ") {
		return errors.New("API key must be 8 to 512 characters with no whitespace")
	}
	lock, err := acquireMaintenanceLock(dir)
	if err != nil {
		return err
	}
	defer lock.release()
	path := filepath.Join(dir, ".env")
	old, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated, err := replaceEnvironment(old, map[string]string{"BARQ_API_KEY": raw})
	if err != nil {
		return err
	}
	return writeFile(dir, ".env", updated, 0o600)
}
