package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/barqdb/barq-server/internal/control"
	"github.com/barqdb/barq-server/internal/dataplane"
)

const (
	apiKeyCollection = "api_keys"
	tenantCollection = "tenants"
)

var (
	identityPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)
	actionPattern   = regexp.MustCompile(`^[a-z][a-z0-9_-]*(?::[a-z][a-z0-9_-]*)?$`)
)

type Manager struct {
	store control.Store
	mu    sync.Mutex
}

type BootstrapOptions struct {
	APIKeys         string
	DevMode         bool
	DefaultTenant   string
	DefaultDatabase string
}

type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Databases []string  `json:"databases"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateTenantInput struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Databases []string `json:"databases"`
}

type UpdateTenantInput struct {
	Name      *string   `json:"name,omitempty"`
	Databases *[]string `json:"databases,omitempty"`
	Enabled   *bool     `json:"enabled,omitempty"`
}

type CreateServiceKeyInput struct {
	Label    string   `json:"label"`
	Tenant   string   `json:"tenant"`
	Database string   `json:"database"`
	Actions  []string `json:"actions"`
}

type UpdateServiceKeyInput struct {
	Label   *string   `json:"label,omitempty"`
	Actions *[]string `json:"actions,omitempty"`
	Enabled *bool     `json:"enabled,omitempty"`
}

type ServiceKeyView struct {
	ID        string    `json:"id"`
	Label     string    `json:"label,omitempty"`
	Tenant    string    `json:"tenant"`
	Database  string    `json:"database"`
	Actions   []string  `json:"actions"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreatedServiceKey struct {
	Key    ServiceKeyView `json:"key"`
	Secret string         `json:"secret"`
}

func NewManager(store control.Store) *Manager { return &Manager{store: store} }

// Bootstrap writes only key digests to control.barq. Once at least one key is
// stored, environment key configuration is no longer read or reapplied.
func (m *Manager) Bootstrap(ctx context.Context, options BootstrapOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	records, err := m.store.List(ctx, apiKeyCollection, "")
	if err != nil {
		return err
	}
	if len(records) != 0 {
		// Stored control state is authoritative after the first successful start.
		// Rebuild tenants only for an early upgrade that has stored key records but
		// no tenant registry at all. Never overwrite later console changes.
		tenants, err := m.store.List(ctx, tenantCollection, "")
		if err != nil || len(tenants) != 0 {
			return err
		}
		return m.ensureBootstrapTenants(ctx, records, options.DefaultTenant, options.DefaultDatabase)
	}

	config := strings.TrimSpace(options.APIKeys)
	if config == "" && options.DevMode {
		config = "dev-key:*:*:*"
		if options.DefaultTenant == "" {
			options.DefaultTenant = "dev"
		}
		if options.DefaultDatabase == "" {
			options.DefaultDatabase = "default"
		}
	}
	if config == "" {
		return fmt.Errorf("BARQ_API_KEYS is required for the first start")
	}

	now := time.Now().UTC()
	entries := strings.Split(config, ",")
	if len(entries) > 50 {
		return invalid("at most 50 bootstrap API keys are allowed")
	}
	keys := make([]ServiceKey, 0, len(entries))
	mutations := make([]control.Mutation, 0, len(entries))
	for _, entry := range entries {
		raw, key, err := parseBootstrapKey(entry, now)
		if err != nil {
			return err
		}
		key.Digest = Digest(raw)
		key.ID = "key_" + key.Digest[:16]
		value, err := control.Encode(key)
		if err != nil {
			return err
		}
		zero := uint64(0)
		mutations = append(mutations, control.Mutation{Op: control.MutationPut, Collection: apiKeyCollection, Key: key.Digest, Value: value, ExpectedVersion: &zero})
		keys = append(keys, key)
	}
	if _, err := m.store.Apply(ctx, mutations); err != nil {
		return fmt.Errorf("store bootstrap API keys: %w", err)
	}
	records = make([]control.Record, 0, len(keys))
	for _, key := range keys {
		value, _ := control.Encode(key)
		records = append(records, control.Record{Value: value})
	}
	return m.ensureBootstrapTenants(ctx, records, options.DefaultTenant, options.DefaultDatabase)
}

func (m *Manager) ensureBootstrapTenants(ctx context.Context, keyRecords []control.Record, defaultTenant, defaultDatabase string) error {
	databaseSets := map[string]map[string]bool{}
	add := func(tenant, database string) {
		if tenant == "" || database == "" || tenant == "*" || database == "*" {
			return
		}
		if databaseSets[tenant] == nil {
			databaseSets[tenant] = map[string]bool{}
		}
		databaseSets[tenant][database] = true
	}
	add(defaultTenant, defaultDatabase)
	for _, record := range keyRecords {
		key, err := control.Decode[ServiceKey](record)
		if err != nil {
			return fmt.Errorf("read stored API key: %w", err)
		}
		add(key.Tenant, key.Database)
	}
	for tenantID, set := range databaseSets {
		if !validIdentity(tenantID) {
			return invalid("invalid bootstrap tenant")
		}
		databases := make([]string, 0, len(set))
		for database := range set {
			if !validIdentity(database) {
				return invalid("invalid bootstrap database")
			}
			databases = append(databases, database)
		}
		sort.Strings(databases)
		record, err := m.store.Get(ctx, tenantCollection, tenantID)
		if dataplane.IsCode(err, dataplane.CodeNotFound) {
			now := time.Now().UTC()
			tenant := Tenant{ID: tenantID, Name: tenantID, Databases: databases, Enabled: true, CreatedAt: now, UpdatedAt: now}
			value, encodeErr := control.Encode(tenant)
			if encodeErr != nil {
				return encodeErr
			}
			zero := uint64(0)
			if _, putErr := m.store.Put(ctx, tenantCollection, tenantID, value, &zero); putErr != nil {
				return putErr
			}
			continue
		}
		if err != nil {
			return err
		}
		tenant, err := control.Decode[Tenant](record)
		if err != nil {
			return err
		}
		merged := append([]string(nil), tenant.Databases...)
		seen := map[string]bool{}
		for _, database := range merged {
			seen[database] = true
		}
		for _, database := range databases {
			if !seen[database] {
				merged = append(merged, database)
			}
		}
		sort.Strings(merged)
		if len(merged) != len(tenant.Databases) {
			tenant.Databases = merged
			tenant.UpdatedAt = time.Now().UTC()
			value, _ := control.Encode(tenant)
			if _, err := m.store.Put(ctx, tenantCollection, tenantID, value, &record.Version); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) FindByDigest(ctx context.Context, digest string) (ServiceKey, error) {
	record, err := m.store.Get(ctx, apiKeyCollection, digest)
	if err != nil {
		if dataplane.IsCode(err, dataplane.CodeNotFound) {
			return ServiceKey{}, unauthorized()
		}
		return ServiceKey{}, err
	}
	key, err := control.Decode[ServiceKey](record)
	if err != nil {
		return ServiceKey{}, &dataplane.Error{Code: dataplane.CodeInternal, Message: "invalid stored API key"}
	}
	if !key.Enabled {
		return ServiceKey{}, unauthorized()
	}
	if key.Tenant != "*" {
		tenant, _, err := m.getTenant(ctx, key.Tenant)
		if err != nil || !tenant.Enabled || (key.Database != "*" && !contains(tenant.Databases, key.Database)) {
			return ServiceKey{}, unauthorized()
		}
	}
	return key, nil
}

func (m *Manager) ListTenants(ctx context.Context) ([]Tenant, error) {
	records, err := m.store.List(ctx, tenantCollection, "")
	if err != nil {
		return nil, err
	}
	tenants := make([]Tenant, 0, len(records))
	for _, record := range records {
		tenant, err := control.Decode[Tenant](record)
		if err != nil {
			return nil, fmt.Errorf("read tenant %s: %w", record.Key, err)
		}
		tenants = append(tenants, tenant)
	}
	sort.Slice(tenants, func(i, j int) bool { return tenants[i].ID < tenants[j].ID })
	return tenants, nil
}

func (m *Manager) GetTenant(ctx context.Context, id string) (Tenant, error) {
	tenant, _, err := m.getTenant(ctx, id)
	return tenant, err
}

func (m *Manager) CreateTenant(ctx context.Context, input CreateTenantInput) (Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	if !validIdentity(input.ID) {
		return Tenant{}, invalid("tenant id must use letters, numbers, dashes, or underscores")
	}
	if input.Name == "" || len(input.Name) > 120 {
		return Tenant{}, invalid("tenant name must be 1 to 120 characters")
	}
	databases, err := normalizeDatabases(input.Databases)
	if err != nil {
		return Tenant{}, err
	}
	now := time.Now().UTC()
	tenant := Tenant{ID: input.ID, Name: input.Name, Databases: databases, Enabled: true, CreatedAt: now, UpdatedAt: now}
	value, err := control.Encode(tenant)
	if err != nil {
		return Tenant{}, err
	}
	zero := uint64(0)
	if _, err := m.store.Put(ctx, tenantCollection, tenant.ID, value, &zero); err != nil {
		return Tenant{}, err
	}
	return tenant, nil
}

func (m *Manager) UpdateTenant(ctx context.Context, id string, input UpdateTenantInput) (Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if input.Name == nil && input.Databases == nil && input.Enabled == nil {
		return Tenant{}, invalid("tenant update has no changes")
	}
	tenant, version, err := m.getTenant(ctx, id)
	if err != nil {
		return Tenant{}, err
	}
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if name == "" || len(name) > 120 {
			return Tenant{}, invalid("tenant name must be 1 to 120 characters")
		}
		tenant.Name = name
	}
	if input.Databases != nil {
		tenant.Databases, err = normalizeDatabases(*input.Databases)
		if err != nil {
			return Tenant{}, err
		}
	}
	if input.Enabled != nil {
		tenant.Enabled = *input.Enabled
	}
	tenant.UpdatedAt = time.Now().UTC()
	value, _ := control.Encode(tenant)
	if _, err := m.store.Put(ctx, tenantCollection, tenant.ID, value, &version); err != nil {
		return Tenant{}, err
	}
	return tenant, nil
}

func (m *Manager) DisableTenant(ctx context.Context, id string) error {
	disabled := false
	_, err := m.UpdateTenant(ctx, id, UpdateTenantInput{Enabled: &disabled})
	return err
}

func (m *Manager) Scopes(ctx context.Context) ([]dataplane.Scope, error) {
	tenants, err := m.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	var scopes []dataplane.Scope
	for _, tenant := range tenants {
		if !tenant.Enabled {
			continue
		}
		for _, database := range tenant.Databases {
			scopes = append(scopes, dataplane.Scope{Tenant: tenant.ID, Database: database})
		}
	}
	return scopes, nil
}

func (m *Manager) ListKeys(ctx context.Context) ([]ServiceKey, error) {
	records, err := m.store.List(ctx, apiKeyCollection, "")
	if err != nil {
		return nil, err
	}
	keys := make([]ServiceKey, 0, len(records))
	for _, record := range records {
		key, err := control.Decode[ServiceKey](record)
		if err != nil {
			return nil, fmt.Errorf("read API key: %w", err)
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys, nil
}

func (m *Manager) GetKey(ctx context.Context, id string) (ServiceKey, error) {
	key, _, err := m.getKey(ctx, id)
	return key, err
}

func (m *Manager) CreateKey(ctx context.Context, input CreateServiceKeyInput) (CreatedServiceKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateKeyInput(ctx, &input); err != nil {
		return CreatedServiceKey{}, err
	}
	key, raw, err := m.newKey(ctx, input, time.Now().UTC())
	if err != nil {
		return CreatedServiceKey{}, err
	}
	value, _ := control.Encode(key)
	zero := uint64(0)
	if _, err := m.store.Put(ctx, apiKeyCollection, key.Digest, value, &zero); err != nil {
		return CreatedServiceKey{}, err
	}
	return CreatedServiceKey{Key: keyView(key), Secret: raw}, nil
}

func (m *Manager) UpdateKey(ctx context.Context, id string, input UpdateServiceKeyInput) (ServiceKeyView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if input.Label == nil && input.Actions == nil && input.Enabled == nil {
		return ServiceKeyView{}, invalid("API key update has no changes")
	}
	key, record, err := m.getKey(ctx, id)
	if err != nil {
		return ServiceKeyView{}, err
	}
	wasRoot := isRootKey(key)
	if input.Label != nil {
		key.Label = strings.TrimSpace(*input.Label)
		if len(key.Label) > 120 {
			return ServiceKeyView{}, invalid("key label must be at most 120 characters")
		}
	}
	if input.Actions != nil {
		key.Actions, err = normalizeActions(*input.Actions)
		if err != nil {
			return ServiceKeyView{}, err
		}
	}
	if input.Enabled != nil {
		key.Enabled = *input.Enabled
		if key.Enabled {
			if err := m.validateScope(ctx, key.Tenant, key.Database); err != nil {
				return ServiceKeyView{}, err
			}
		}
	}
	if wasRoot && !isRootKey(key) {
		if err := m.ensureAnotherRoot(ctx, id); err != nil {
			return ServiceKeyView{}, err
		}
	}
	key.UpdatedAt = time.Now().UTC()
	value, _ := control.Encode(key)
	if _, err := m.store.Put(ctx, apiKeyCollection, key.Digest, value, &record.Version); err != nil {
		return ServiceKeyView{}, err
	}
	return keyView(key), nil
}

// Revoke keeps metadata for audit and makes the secret unusable immediately.
func (m *Manager) RevokeKey(ctx context.Context, id string) error {
	disabled := false
	_, err := m.UpdateKey(ctx, id, UpdateServiceKeyInput{Enabled: &disabled})
	return err
}

// Rotate atomically disables the old digest and creates a replacement. The
// replacement secret is returned once and is never stored.
func (m *Manager) RotateKey(ctx context.Context, id string) (CreatedServiceKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, record, err := m.getKey(ctx, id)
	if err != nil {
		return CreatedServiceKey{}, err
	}
	if !old.Enabled {
		return CreatedServiceKey{}, &dataplane.Error{Code: dataplane.CodeConflict, Message: "only an enabled API key can be rotated"}
	}
	if err := m.validateScope(ctx, old.Tenant, old.Database); err != nil {
		return CreatedServiceKey{}, err
	}
	input := CreateServiceKeyInput{Label: old.Label, Tenant: old.Tenant, Database: old.Database, Actions: old.Actions}
	replacement, raw, err := m.newKey(ctx, input, time.Now().UTC())
	if err != nil {
		return CreatedServiceKey{}, err
	}
	old.Enabled = false
	old.UpdatedAt = replacement.CreatedAt
	oldValue, _ := control.Encode(old)
	newValue, _ := control.Encode(replacement)
	zero := uint64(0)
	_, err = m.store.Apply(ctx, []control.Mutation{
		{Op: control.MutationPut, Collection: apiKeyCollection, Key: old.Digest, Value: oldValue, ExpectedVersion: &record.Version},
		{Op: control.MutationPut, Collection: apiKeyCollection, Key: replacement.Digest, Value: newValue, ExpectedVersion: &zero},
	})
	if err != nil {
		return CreatedServiceKey{}, err
	}
	return CreatedServiceKey{Key: keyView(replacement), Secret: raw}, nil
}

func (m *Manager) getTenant(ctx context.Context, id string) (Tenant, uint64, error) {
	record, err := m.store.Get(ctx, tenantCollection, id)
	if err != nil {
		return Tenant{}, 0, err
	}
	tenant, err := control.Decode[Tenant](record)
	return tenant, record.Version, err
}

func (m *Manager) getKey(ctx context.Context, id string) (ServiceKey, control.Record, error) {
	records, err := m.store.List(ctx, apiKeyCollection, "")
	if err != nil {
		return ServiceKey{}, control.Record{}, err
	}
	for _, record := range records {
		key, err := control.Decode[ServiceKey](record)
		if err != nil {
			return ServiceKey{}, control.Record{}, err
		}
		if key.ID == id {
			return key, record, nil
		}
	}
	return ServiceKey{}, control.Record{}, &dataplane.Error{Code: dataplane.CodeNotFound, Message: "API key not found"}
}

func (m *Manager) validateKeyInput(ctx context.Context, input *CreateServiceKeyInput) error {
	input.Label = strings.TrimSpace(input.Label)
	input.Tenant = strings.TrimSpace(input.Tenant)
	input.Database = strings.TrimSpace(input.Database)
	if input.Label == "" || len(input.Label) > 120 {
		return invalid("key label must be 1 to 120 characters")
	}
	actions, err := normalizeActions(input.Actions)
	if err != nil {
		return err
	}
	input.Actions = actions
	return m.validateScope(ctx, input.Tenant, input.Database)
}

func (m *Manager) validateScope(ctx context.Context, tenantID, database string) error {
	if tenantID == "*" {
		if database != "*" {
			return invalid("a global key must use * for both tenant and database")
		}
		return nil
	}
	if !validIdentity(tenantID) || (database != "*" && !validIdentity(database)) {
		return invalid("invalid key tenant or database")
	}
	tenant, _, err := m.getTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	if !tenant.Enabled {
		return invalid("cannot create or enable a key for a disabled tenant")
	}
	if database != "*" && !contains(tenant.Databases, database) {
		return invalid("database is not registered for this tenant")
	}
	return nil
}

func (m *Manager) newKey(ctx context.Context, input CreateServiceKeyInput, now time.Time) (ServiceKey, string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		secretBytes := make([]byte, 24)
		idBytes := make([]byte, 10)
		if _, err := rand.Read(secretBytes); err != nil {
			return ServiceKey{}, "", err
		}
		if _, err := rand.Read(idBytes); err != nil {
			return ServiceKey{}, "", err
		}
		raw := "barq_sk_" + base64.RawURLEncoding.EncodeToString(secretBytes)
		key := ServiceKey{
			ID: "key_" + hex.EncodeToString(idBytes), Label: input.Label, Digest: Digest(raw),
			Tenant: input.Tenant, Database: input.Database, Actions: append([]string(nil), input.Actions...),
			Enabled: true, CreatedAt: now, UpdatedAt: now,
		}
		if _, err := m.store.Get(ctx, apiKeyCollection, key.Digest); dataplane.IsCode(err, dataplane.CodeNotFound) {
			return key, raw, nil
		} else if err != nil {
			return ServiceKey{}, "", err
		}
	}
	return ServiceKey{}, "", fmt.Errorf("could not generate a unique API key")
}

func (m *Manager) ensureAnotherRoot(ctx context.Context, excludedID string) error {
	keys, err := m.ListKeys(ctx)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if key.ID != excludedID && isRootKey(key) {
			return nil
		}
	}
	return &dataplane.Error{Code: dataplane.CodeConflict, Message: "cannot revoke the last global admin key"}
}

func parseBootstrapKey(entry string, now time.Time) (string, ServiceKey, error) {
	parts := strings.SplitN(strings.TrimSpace(entry), ":", 4)
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return "", ServiceKey{}, fmt.Errorf("invalid BARQ_API_KEYS entry; expected key:tenant:database:action|action")
	}
	actions, err := normalizeActions(strings.Split(parts[3], "|"))
	if err != nil {
		return "", ServiceKey{}, err
	}
	if parts[1] != "*" && !validIdentity(parts[1]) || parts[2] != "*" && !validIdentity(parts[2]) {
		return "", ServiceKey{}, invalid("invalid bootstrap key scope")
	}
	if parts[1] == "*" && parts[2] != "*" {
		return "", ServiceKey{}, invalid("a global key must use * for both tenant and database")
	}
	return parts[0], ServiceKey{Tenant: parts[1], Database: parts[2], Actions: actions, Enabled: true, CreatedAt: now, UpdatedAt: now}, nil
}

func normalizeDatabases(input []string) ([]string, error) {
	if len(input) == 0 || len(input) > 50 {
		return nil, invalid("a tenant needs 1 to 50 databases")
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(input))
	for _, item := range input {
		item = strings.TrimSpace(item)
		if !validIdentity(item) {
			return nil, invalid("database names must use letters, numbers, dashes, or underscores")
		}
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	sort.Strings(result)
	return result, nil
}

func normalizeActions(input []string) ([]string, error) {
	if len(input) == 0 || len(input) > 20 {
		return nil, invalid("an API key needs 1 to 20 actions")
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(input))
	for _, action := range input {
		action = strings.TrimSpace(action)
		if action != "*" && !actionPattern.MatchString(action) {
			return nil, invalid("invalid API key action")
		}
		if !seen[action] {
			seen[action] = true
			result = append(result, action)
		}
	}
	sort.Strings(result)
	return result, nil
}

func keyView(key ServiceKey) ServiceKeyView {
	return ServiceKeyView{ID: key.ID, Label: key.Label, Tenant: key.Tenant, Database: key.Database, Actions: append([]string(nil), key.Actions...), Enabled: key.Enabled, CreatedAt: key.CreatedAt, UpdatedAt: key.UpdatedAt}
}

func PublicKey(key ServiceKey) ServiceKeyView { return keyView(key) }

func isRootKey(key ServiceKey) bool {
	return key.Enabled && key.Tenant == "*" && key.Database == "*" && contains(key.Actions, "*")
}

func validIdentity(value string) bool { return identityPattern.MatchString(value) }

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func invalid(message string) error {
	return &dataplane.Error{Code: dataplane.CodeInvalid, Message: message}
}

func unauthorized() error {
	return &dataplane.Error{Code: dataplane.CodeUnauthorized, Message: "invalid API key"}
}
