package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
	"github.com/pocketbase/pocketbase/tools/security"
	"golang.org/x/crypto/pbkdf2"

	"github.com/gera2ld/prism/internal/gateway"
)

type providerConfig struct {
	name    string
	baseURL string
	apiKey  string
}

type Store struct {
	app        core.App
	encryption []byte
	logger     *slog.Logger

	mu      sync.Mutex
	targets map[string][]gateway.Target
	models  []string
	keys    map[string]gateway.Key

	settingsLoaded bool
	captureBodies  bool
	retentionHours int
	retentionCron  string

	transformers       []gateway.Transformer
	transformersLoaded bool

	onSettingsChange func()
}

const pbkdf2Iterations = 600000

// encryptionSalt is app-scoped domain separation for the PBKDF2-derived
// AES-256 key. Intentionally not tied to the repo URL so renames don't
// force re-derivation.
const encryptionSalt = "prism-gateway-encryption-v1"

func Open(app core.App, logger *slog.Logger) (*Store, error) {
	secret := os.Getenv("GATEWAY_ENCRYPTION_KEY")
	if secret == "" {
		return nil, errors.New("GATEWAY_ENCRYPTION_KEY must be set")
	}
	s := &Store{
		app:        app,
		encryption: pbkdf2.Key([]byte(secret), []byte(encryptionSalt), pbkdf2Iterations, 32, sha256.New),
		logger:     logger,
		targets:    map[string][]gateway.Target{},
		keys:       map[string]gateway.Key{},
	}
	invalidate := &hook.Handler[*core.RecordEvent]{
		Func: func(e *core.RecordEvent) error {
			s.Invalidate()
			return e.Next()
		},
	}
	app.OnRecordAfterCreateSuccess("providers", "routes").Bind(invalidate)
	app.OnRecordAfterUpdateSuccess("providers", "routes").Bind(invalidate)
	app.OnRecordAfterDeleteSuccess("providers", "routes").Bind(invalidate)

	invalidateSettings := &hook.Handler[*core.RecordEvent]{
		Func: func(e *core.RecordEvent) error {
			s.InvalidateSettings()
			return e.Next()
		},
	}
	app.OnRecordAfterCreateSuccess(settingsCollection).Bind(invalidateSettings)
	app.OnRecordAfterUpdateSuccess(settingsCollection).Bind(invalidateSettings)
	app.OnRecordAfterDeleteSuccess(settingsCollection).Bind(invalidateSettings)

	invalidateTransformers := &hook.Handler[*core.RecordEvent]{
		Func: func(e *core.RecordEvent) error {
			s.InvalidateTransformers()
			return e.Next()
		},
	}
	app.OnRecordAfterCreateSuccess("transformers").Bind(invalidateTransformers)
	app.OnRecordAfterUpdateSuccess("transformers").Bind(invalidateTransformers)
	app.OnRecordAfterDeleteSuccess("transformers").Bind(invalidateTransformers)

	validate := &hook.Handler[*core.RecordEvent]{
		Func: func(e *core.RecordEvent) error {
			if err := validateTransformer(e); err != nil {
				return err
			}
			return e.Next()
		},
	}
	app.OnRecordCreate("transformers").Bind(validate)
	app.OnRecordUpdate("transformers").Bind(validate)

	encryptKey := &hook.Handler[*core.RecordEvent]{
		Func: func(e *core.RecordEvent) error {
			if e.Record.Collection().Name != "providers" {
				return e.Next()
			}
			plain := e.Record.GetString("api_key")
			if plain == "" || strings.HasPrefix(plain, "enc:") {
				return e.Next()
			}
			blob, err := security.Encrypt([]byte(plain), string(s.encryption))
			if err != nil {
				return err
			}
			e.Record.Set("api_key", "enc:"+blob)
			return e.Next()
		},
	}
	app.OnRecordCreate("providers").Bind(encryptKey)
	app.OnRecordUpdate("providers").Bind(encryptKey)

	// api_keys: leaving key_plain empty autogenerates a secret (create) or
	// regenerates it (update); any plaintext present is hashed into key_hash
	// and encrypted at rest. Already-locked (enc:) values pass through, so
	// CreateAPIKey and name-only edits are unaffected.
	apiKeySecret := &hook.Handler[*core.RecordEvent]{
		Func: func(e *core.RecordEvent) error {
			if err := s.normalizeAPIKey(e.Record); err != nil {
				return err
			}
			return e.Next()
		},
	}
	app.OnRecordCreate("api_keys").Bind(apiKeySecret)
	app.OnRecordUpdate("api_keys").Bind(apiKeySecret)

	validateKeyPolicy := &hook.Handler[*core.RecordEvent]{
		Func: func(e *core.RecordEvent) error {
			_, _, _, err := gateway.CompilePolicy(
				e.Record.GetString("alias_pattern"),
				e.Record.GetString("provider_pattern"),
				e.Record.GetString("model_pattern"),
			)
			if err != nil {
				return err
			}
			return e.Next()
		},
	}
	app.OnRecordCreate("api_keys").Bind(validateKeyPolicy)
	app.OnRecordUpdate("api_keys").Bind(validateKeyPolicy)

	// api_keys edits must drop cached authentications immediately, or a
	// disabled key (or tightened policy) would keep working from cache.
	invalidateKeys := &hook.Handler[*core.RecordEvent]{
		Func: func(e *core.RecordEvent) error {
			s.InvalidateKeys()
			return e.Next()
		},
	}
	app.OnRecordAfterCreateSuccess("api_keys").Bind(invalidateKeys)
	app.OnRecordAfterUpdateSuccess("api_keys").Bind(invalidateKeys)
	app.OnRecordAfterDeleteSuccess("api_keys").Bind(invalidateKeys)

	// Reconcile gateway_settings with the schema on every start: missing
	// fields/defaults are generated, redundant fields removed.
	if err := s.reconcileSettings(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targets = map[string][]gateway.Target{}
	s.models = nil
	s.keys = map[string]gateway.Key{}
}

func (s *Store) InvalidateKeys() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = map[string]gateway.Key{}
}

func (s *Store) InvalidateSettings() {
	s.mu.Lock()
	s.settingsLoaded = false
	cb := s.onSettingsChange
	s.mu.Unlock()
	// Run outside the lock: the callback reads settings back (re-locking).
	if cb != nil {
		cb()
	}
}

// OnSettingsChange registers a callback invoked synchronously after every
// gateway_settings change. Used to reschedule the retention cron.
func (s *Store) OnSettingsChange(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onSettingsChange = fn
}

// CaptureEnabled reports the global capture_bodies setting. Cached in
// memory, invalidated by gateway_settings record hooks so admin UI edits
// apply to the next request without a restart.
func (s *Store) CaptureEnabled() bool {
	if err := s.loadSettings(); err != nil && s.logger != nil {
		s.logger.Error("failed to load settings", "error", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captureBodies
}

// RetentionCron reports the cleanup job schedule. Same caching contract
// as CaptureEnabled. An invalid expression falls back to the default so a
// typo never kills cleanup; UpdateSchedule logs and keeps the old job.
func (s *Store) RetentionCron() string {
	if err := s.loadSettings(); err != nil && s.logger != nil {
		s.logger.Error("failed to load settings", "error", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retentionCron == "" {
		return defaultRetentionCron
	}
	return s.retentionCron
}

// Retention reports how long request_bodies rows are kept. Same caching
// contract as CaptureEnabled.
func (s *Store) Retention() time.Duration {
	if err := s.loadSettings(); err != nil && s.logger != nil {
		s.logger.Error("failed to load settings", "error", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retentionHours <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(s.retentionHours) * time.Hour
}

// loadSettings fills the in-memory cache from the typed Settings manager.
// Read-only: persisting defaults is reconcileSettings' job on start.
func (s *Store) loadSettings() error {
	s.mu.Lock()
	if s.settingsLoaded {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	settings, _ := readSettings(s.app)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.captureBodies = settings.CaptureBodies
	s.retentionHours = settings.RetentionHours
	s.retentionCron = settings.RetentionCron
	s.settingsLoaded = true
	return nil
}

// normalizeAPIKey implements the dashboard key flow: an empty secret is
// autogenerated (create) or regenerated (update); any plaintext present is
// hashed into key_hash and encrypted into key_plain. Locked (enc:) values
// pass through untouched.
func (s *Store) normalizeAPIKey(record *core.Record) error {
	plain := record.GetString("key_plain")
	if plain == "" {
		plain = "sk-" + security.RandomString(40)
	}
	if strings.HasPrefix(plain, "enc:") {
		return nil
	}
	sum := sha256.Sum256([]byte(plain))
	record.Set("key_hash", hex.EncodeToString(sum[:]))
	blob, err := security.Encrypt([]byte(plain), string(s.encryption))
	if err != nil {
		return err
	}
	record.Set("key_plain", "enc:"+blob)
	return nil
}

func (s *Store) decrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	blob, ok := strings.CutPrefix(ciphertext, "enc:")
	if !ok {
		return "", errors.New("provider API key is not encrypted")
	}
	plaintext, err := security.Decrypt(blob, string(s.encryption))
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func (s *Store) Authenticate(ctx context.Context, presented string) (gateway.Key, error) {
	sum := sha256.Sum256([]byte(presented))
	hash := hex.EncodeToString(sum[:])

	s.mu.Lock()
	cached, ok := s.keys[hash]
	s.mu.Unlock()
	if ok {
		return cached, nil
	}

	record, err := s.app.FindFirstRecordByFilter("api_keys", "key_hash = {:hash} && enabled = true", dbx.Params{"hash": hash})
	if err != nil {
		return gateway.Key{}, gateway.ErrUnauthorized
	}
	aliasPattern, providerPattern, modelPattern, err := gateway.CompilePolicy(
		record.GetString("alias_pattern"),
		record.GetString("provider_pattern"),
		record.GetString("model_pattern"),
	)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("key has invalid policy; denying", "key", record.GetString("name"), "error", err)
		}
		return gateway.Key{}, gateway.ErrForbidden
	}
	key := gateway.Key{ID: record.Id, Name: record.GetString("name"),
		AliasPattern: aliasPattern, ProviderPattern: providerPattern, ModelPattern: modelPattern}

	s.mu.Lock()
	s.keys[hash] = key
	s.mu.Unlock()
	return key, nil
}

// Resolve maps an alias to its priority-ordered targets. Every model must
// go through the routing table; unknown aliases are an error, no exceptions.
func (s *Store) Resolve(ctx context.Context, alias string) ([]gateway.Target, error) {
	if err := s.loadRoutes(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	targets := s.targets[alias]
	if len(targets) == 0 {
		return nil, gateway.ErrUnknownModel
	}
	return slices.Clone(targets), nil
}

func (s *Store) Models(ctx context.Context) ([]string, error) {
	if err := s.loadRoutes(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.models), nil
}

func (s *Store) CreateAPIKey(app core.App, name string) (string, error) {
	secret := "sk-" + security.RandomString(40)
	sum := sha256.Sum256([]byte(secret))
	collection, err := app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		return "", err
	}
	record := core.NewRecord(collection)
	record.Set("name", name)
	record.Set("key_hash", hex.EncodeToString(sum[:]))
	blob, err := security.Encrypt([]byte(secret), string(s.encryption))
	if err != nil {
		return "", err
	}
	record.Set("key_plain", "enc:"+blob)
	record.Set("enabled", true)
	if err := app.Save(record); err != nil {
		return "", err
	}
	return secret, nil
}

// ProviderInfo is the non-secret view of a provider. The upstream token is
// never exposed here; use RevealProviderToken to copy it.
type ProviderInfo struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Enabled bool   `json:"enabled"`
}

// ListProviders returns all providers sorted by name.
func (s *Store) ListProviders(app core.App) ([]ProviderInfo, error) {
	records, err := app.FindAllRecords("providers")
	if err != nil {
		return nil, err
	}
	providers := make([]ProviderInfo, 0, len(records))
	for _, r := range records {
		providers = append(providers, ProviderInfo{
			Name:    r.GetString("name"),
			BaseURL: r.GetString("base_url"),
			Enabled: r.GetBool("enabled"),
		})
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Name < providers[j].Name })
	return providers, nil
}

// GetProvider returns the non-secret view of one provider by name.
func (s *Store) GetProvider(app core.App, name string) (ProviderInfo, error) {
	record, err := app.FindFirstRecordByFilter("providers", "name = {:name}", dbx.Params{"name": name})
	if err != nil {
		return ProviderInfo{}, err
	}
	return ProviderInfo{
		Name:    record.GetString("name"),
		BaseURL: record.GetString("base_url"),
		Enabled: record.GetBool("enabled"),
	}, nil
}

// RevealProviderToken decrypts the stored upstream token for copying, e.g.
// via the provider reveal command.
func (s *Store) RevealProviderToken(app core.App, name string) (string, error) {
	record, err := app.FindFirstRecordByFilter("providers", "name = {:name}", dbx.Params{"name": name})
	if err != nil {
		return "", err
	}
	if plain := record.GetString("api_key"); plain != "" && !strings.HasPrefix(plain, "enc:") {
		return plain, nil
	}
	token, err := s.decrypt(record.GetString("api_key"))
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", errors.New("provider has no token stored")
	}
	return token, nil
}

// APIKeyInfo is the non-secret listing view of a client key. Secrets and
// hashes are never exposed here; use RevealAPIKey to copy a secret.
type APIKeyInfo struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// ListAPIKeys returns all client keys sorted by name.
func (s *Store) ListAPIKeys(app core.App) ([]APIKeyInfo, error) {
	records, err := app.FindAllRecords("api_keys")
	if err != nil {
		return nil, err
	}
	keys := make([]APIKeyInfo, 0, len(records))
	for _, r := range records {
		keys = append(keys, APIKeyInfo{Name: r.GetString("name"), Enabled: r.GetBool("enabled")})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
	return keys, nil
}

// RevealAPIKey decrypts the stored client secret for copying, e.g. via the
// key reveal command. Auth never uses this path.
func (s *Store) RevealAPIKey(app core.App, name string) (string, error) {
	record, err := app.FindFirstRecordByFilter("api_keys", "name = {:name}", dbx.Params{"name": name})
	if err != nil {
		return "", err
	}
	if plain := record.GetString("key_plain"); plain != "" && !strings.HasPrefix(plain, "enc:") {
		return plain, nil
	}
	return s.decrypt(record.GetString("key_plain"))
}

func (s *Store) loadRoutes() error {
	s.mu.Lock()
	if s.models != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	providers, err := s.app.FindAllRecords("providers", dbx.HashExp{"enabled": true})
	if err != nil {
		return err
	}
	byID := make(map[string]providerConfig, len(providers))
	for _, p := range providers {
		name := p.GetString("name")
		key, decErr := s.decrypt(p.GetString("api_key"))
		if decErr != nil {
			return decErr
		}
		byID[p.Id] = providerConfig{name: name, baseURL: p.GetString("base_url"), apiKey: key}
	}

	routes, err := s.app.FindAllRecords("routes", dbx.HashExp{"enabled": true})
	if err != nil {
		return err
	}
	slices.SortStableFunc(routes, func(a, b *core.Record) int {
		return cmp.Compare(a.GetFloat("priority"), b.GetFloat("priority"))
	})
	targets := make(map[string][]gateway.Target, len(routes))
	for _, r := range routes {
		p, ok := byID[r.GetString("provider")]
		if !ok {
			continue
		}
		alias := r.GetString("alias")
		targets[alias] = append(targets[alias], gateway.Target{
			ProviderID:   r.GetString("provider"),
			ProviderName: p.name,
			BaseURL:      p.baseURL,
			APIKey:       p.apiKey,
			Model:        r.GetString("upstream_model"),
		})
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.targets = targets
	aliases := make([]string, 0, len(targets))
	for alias := range targets {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	s.models = aliases
	s.keys = map[string]gateway.Key{}
	return nil
}
