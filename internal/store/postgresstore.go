package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
	"proxycore/api/v6/internal/misc"
	cliproxyauth "proxycore/api/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	defaultConfigTable  = "config_store"
	defaultAuthTable    = "auth_store"
	defaultConfigKey    = "config"
	defaultAPIKeysTable = "api_keys"
	defaultUsageTable   = "usage_records"
)

// PostgresStoreConfig captures configuration required to initialize a Postgres-backed store.
type PostgresStoreConfig struct {
	DSN         string
	Schema      string
	ConfigTable string
	AuthTable   string
	SpoolDir    string
	NodeIP      string // identifies this machine; used to scope auth records
}

// PostgresStore persists configuration and authentication metadata using PostgreSQL as backend
// while mirroring data to a local workspace so existing file-based workflows continue to operate.
type PostgresStore struct {
	db         *sql.DB
	cfg        PostgresStoreConfig
	spoolRoot  string
	configPath string
	authDir    string
	mu         sync.Mutex

	// usage async writer
	usageCh   chan UsageRecord
	usageStop chan struct{}
}

// NewPostgresStore establishes a connection to PostgreSQL and prepares the local workspace.
func NewPostgresStore(ctx context.Context, cfg PostgresStoreConfig) (*PostgresStore, error) {
	trimmedDSN := strings.TrimSpace(cfg.DSN)
	if trimmedDSN == "" {
		return nil, fmt.Errorf("postgres store: DSN is required")
	}
	cfg.DSN = trimmedDSN
	if cfg.ConfigTable == "" {
		cfg.ConfigTable = defaultConfigTable
	}
	if cfg.AuthTable == "" {
		cfg.AuthTable = defaultAuthTable
	}

	spoolRoot := strings.TrimSpace(cfg.SpoolDir)
	if spoolRoot == "" {
		if cwd, err := os.Getwd(); err == nil {
			spoolRoot = filepath.Join(cwd, "pgstore")
		} else {
			spoolRoot = filepath.Join(os.TempDir(), "pgstore")
		}
	}
	absSpool, err := filepath.Abs(spoolRoot)
	if err != nil {
		return nil, fmt.Errorf("postgres store: resolve spool directory: %w", err)
	}
	configDir := filepath.Join(absSpool, "config")
	authDir := filepath.Join(absSpool, "auths")
	if err = os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("postgres store: create config directory: %w", err)
	}
	if err = os.MkdirAll(authDir, 0o700); err != nil {
		return nil, fmt.Errorf("postgres store: create auth directory: %w", err)
	}

	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres store: open database connection: %w", err)
	}
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres store: ping database: %w", err)
	}

	store := &PostgresStore{
		db:         db,
		cfg:        cfg,
		spoolRoot:  absSpool,
		configPath: filepath.Join(configDir, "config.yaml"),
		authDir:    authDir,
	}
	store.startUsageWorker()
	return store, nil
}

// Close releases the underlying database connection and stops background workers.
func (s *PostgresStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if s.usageStop != nil {
		close(s.usageStop)
	}
	return s.db.Close()
}

// EnsureSchema creates the required tables (and schema when provided).
func (s *PostgresStore) EnsureSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres store: not initialized")
	}
	if schema := strings.TrimSpace(s.cfg.Schema); schema != "" {
		query := fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", quoteIdentifier(schema))
		if _, err := s.db.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("postgres store: create schema: %w", err)
		}
	}
	configTable := s.fullTableName(s.cfg.ConfigTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id TEXT PRIMARY KEY,
			content TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`, configTable)); err != nil {
		return fmt.Errorf("postgres store: create config table: %w", err)
	}
	authTable := s.fullTableName(s.cfg.AuthTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id TEXT NOT NULL,
			content JSONB NOT NULL,
			node_ip TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (id, node_ip)
		)
	`, authTable)); err != nil {
		return fmt.Errorf("postgres store: create auth table: %w", err)
	}
	// Migrate existing tables that may have the old schema (single-column PK without node_ip).
	// We attempt ALTER TABLE and silently ignore errors (column already exists).
	_, _ = s.db.ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE %s ADD COLUMN IF NOT EXISTS node_ip TEXT NOT NULL DEFAULT ''`,
		authTable,
	))
	_, _ = s.db.ExecContext(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS idx_auth_store_node_ip ON %s (node_ip)`,
		authTable,
	))

	// api_keys table
	apiKeysTable := s.fullTableName(defaultAPIKeysTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			key           TEXT PRIMARY KEY,
			label         TEXT NOT NULL DEFAULT '',
			quota_millions FLOAT8 NOT NULL DEFAULT 0,
			disabled      BOOLEAN NOT NULL DEFAULT FALSE,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`, apiKeysTable)); err != nil {
		return fmt.Errorf("postgres store: create api_keys table: %w", err)
	}

	// usage_records table
	usageTable := s.fullTableName(defaultUsageTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id               BIGSERIAL PRIMARY KEY,
			api_key          TEXT NOT NULL DEFAULT '',
			node_ip          TEXT NOT NULL DEFAULT '',
			provider         TEXT NOT NULL DEFAULT '',
			model            TEXT NOT NULL DEFAULT '',
			auth_id          TEXT NOT NULL DEFAULT '',
			source           TEXT NOT NULL DEFAULT '',
			input_tokens     BIGINT NOT NULL DEFAULT 0,
			output_tokens    BIGINT NOT NULL DEFAULT 0,
			reasoning_tokens BIGINT NOT NULL DEFAULT 0,
			cached_tokens    BIGINT NOT NULL DEFAULT 0,
			total_tokens     BIGINT NOT NULL DEFAULT 0,
			failed           BOOLEAN NOT NULL DEFAULT FALSE,
			requested_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`, usageTable)); err != nil {
		return fmt.Errorf("postgres store: create usage_records table: %w", err)
	}
	_, _ = s.db.ExecContext(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS idx_usage_records_api_key ON %s (api_key)`,
		usageTable,
	))
	_, _ = s.db.ExecContext(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS idx_usage_records_requested_at ON %s (requested_at)`,
		usageTable,
	))
	_, _ = s.db.ExecContext(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS idx_usage_records_node_ip ON %s (node_ip)`,
		usageTable,
	))

	return nil
}

// Bootstrap synchronizes configuration and auth records between PostgreSQL and the local workspace.
func (s *PostgresStore) Bootstrap(ctx context.Context, exampleConfigPath string) error {
	if err := s.EnsureSchema(ctx); err != nil {
		return err
	}
	if err := s.syncConfigFromDatabase(ctx, exampleConfigPath); err != nil {
		return err
	}
	if err := s.ensureDefaultSecretKey(ctx); err != nil {
		log.WithError(err).Warn("failed to ensure default secret key")
	}
	if err := s.syncAuthFromDatabase(ctx); err != nil {
		return err
	}
	return nil
}

// ensureDefaultSecretKey sets secret-key to "admin" (bcrypt hashed) if it is empty in the database config.
func (s *PostgresStore) ensureDefaultSecretKey(ctx context.Context) error {
	query := fmt.Sprintf("SELECT content FROM %s WHERE id = $1", s.fullTableName(s.cfg.ConfigTable))
	var content string
	if err := s.db.QueryRowContext(ctx, query, defaultConfigKey).Scan(&content); err != nil {
		return fmt.Errorf("postgres store: read config for secret key check: %w", err)
	}
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "secret-key:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "secret-key:"))
			val = strings.Trim(val, `"'`)
			if val != "" {
				return nil
			}
		}
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("postgres store: hash default secret key: %w", err)
	}
	updated := updateYAMLScalar(content, []string{"remote-management", "secret-key"}, string(hashed))
	if err = s.persistConfig(ctx, []byte(updated)); err != nil {
		return fmt.Errorf("postgres store: persist default secret key: %w", err)
	}
	if err = os.WriteFile(s.configPath, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("postgres store: write updated config to spool: %w", err)
	}
	log.Info("postgres store: default management secret key set to 'admin'")
	return nil
}

// updateYAMLScalar updates a nested scalar value in a YAML string without a full parse/serialize cycle.
func updateYAMLScalar(content string, path []string, value string) string {
	if len(path) == 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	depth := 0
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		if depth < len(path)-1 {
			trimmed := strings.TrimSpace(line)
			key := path[depth] + ":"
			if strings.HasPrefix(trimmed, key) {
				depth++
			}
		} else if depth == len(path)-1 {
			trimmed := strings.TrimSpace(line)
			key := path[depth] + ":"
			if strings.HasPrefix(trimmed, key) {
				indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				line = indent + path[depth] + `: "` + value + `"`
				depth++
			}
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

// ConfigPath returns the managed configuration file path inside the spool directory.
func (s *PostgresStore) ConfigPath() string {
	if s == nil {
		return ""
	}
	return s.configPath
}

// AuthDir returns the local directory containing mirrored auth files.
func (s *PostgresStore) AuthDir() string {
	if s == nil {
		return ""
	}
	return s.authDir
}

// NodeIP returns the node IP configured for this store instance.
func (s *PostgresStore) NodeIP() string {
	if s == nil {
		return ""
	}
	return s.cfg.NodeIP
}

// WorkDir exposes the root spool directory used for mirroring.
func (s *PostgresStore) WorkDir() string {
	if s == nil {
		return ""
	}
	return s.spoolRoot
}

// SetBaseDir implements the optional interface used by authenticators; it is a no-op because
// the Postgres-backed store controls its own workspace.
func (s *PostgresStore) SetBaseDir(string) {}

// Save persists authentication metadata to disk and PostgreSQL.
func (s *PostgresStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("postgres store: auth is nil")
	}

	path, err := s.resolveAuthPath(auth)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("postgres store: missing file path attribute for %s", auth.ID)
	}

	if auth.Disabled {
		if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
			return "", nil
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("postgres store: create auth directory: %w", err)
	}

	switch {
	case auth.Storage != nil:
		if err = auth.Storage.SaveTokenToFile(path); err != nil {
			return "", err
		}
	case auth.Metadata != nil:
		raw, errMarshal := json.Marshal(auth.Metadata)
		if errMarshal != nil {
			return "", fmt.Errorf("postgres store: marshal metadata: %w", errMarshal)
		}
		if existing, errRead := os.ReadFile(path); errRead == nil {
			if jsonEqual(existing, raw) {
				return path, nil
			}
		} else if errRead != nil && !errors.Is(errRead, fs.ErrNotExist) {
			return "", fmt.Errorf("postgres store: read existing metadata: %w", errRead)
		}
		tmp := path + ".tmp"
		if errWrite := os.WriteFile(tmp, raw, 0o600); errWrite != nil {
			return "", fmt.Errorf("postgres store: write temp auth file: %w", errWrite)
		}
		if errRename := os.Rename(tmp, path); errRename != nil {
			return "", fmt.Errorf("postgres store: rename auth file: %w", errRename)
		}
	default:
		return "", fmt.Errorf("postgres store: nothing to persist for %s", auth.ID)
	}

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["path"] = path

	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = auth.ID
	}

	relID, err := s.relativeAuthID(path)
	if err != nil {
		return "", err
	}
	if err = s.upsertAuthRecord(ctx, relID, path); err != nil {
		return "", err
	}
	return path, nil
}

// List enumerates all auth records stored in PostgreSQL for this node.
func (s *PostgresStore) List(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	query := fmt.Sprintf("SELECT id, content, created_at, updated_at FROM %s WHERE node_ip = $1 ORDER BY id", s.fullTableName(s.cfg.AuthTable))
	rows, err := s.db.QueryContext(ctx, query, s.cfg.NodeIP)
	if err != nil {
		return nil, fmt.Errorf("postgres store: list auth: %w", err)
	}
	defer rows.Close()

	auths := make([]*cliproxyauth.Auth, 0, 32)
	for rows.Next() {
		var (
			id        string
			payload   string
			createdAt time.Time
			updatedAt time.Time
		)
		if err = rows.Scan(&id, &payload, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("postgres store: scan auth row: %w", err)
		}
		path, errPath := s.absoluteAuthPath(id)
		if errPath != nil {
			log.WithError(errPath).Warnf("postgres store: skipping auth %s outside spool", id)
			continue
		}
		metadata := make(map[string]any)
		if err = json.Unmarshal([]byte(payload), &metadata); err != nil {
			log.WithError(err).Warnf("postgres store: skipping auth %s with invalid json", id)
			continue
		}
		provider := strings.TrimSpace(valueAsString(metadata["type"]))
		if provider == "" {
			provider = "unknown"
		}
		attr := map[string]string{"path": path}
		if email := strings.TrimSpace(valueAsString(metadata["email"])); email != "" {
			attr["email"] = email
		}
		auth := &cliproxyauth.Auth{
			ID:               normalizeAuthID(id),
			Provider:         provider,
			FileName:         normalizeAuthID(id),
			Label:            labelFor(metadata),
			Status:           cliproxyauth.StatusActive,
			Attributes:       attr,
			Metadata:         metadata,
			CreatedAt:        createdAt,
			UpdatedAt:        updatedAt,
			LastRefreshedAt:  time.Time{},
			NextRefreshAfter: time.Time{},
		}
		auths = append(auths, auth)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres store: iterate auth rows: %w", err)
	}
	return auths, nil
}

// Delete removes an auth file and the corresponding database record.
func (s *PostgresStore) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("postgres store: id is empty")
	}
	path, err := s.resolveDeletePath(id)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err = os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("postgres store: delete auth file: %w", err)
	}
	relID, err := s.relativeAuthID(path)
	if err != nil {
		return err
	}
	return s.deleteAuthRecord(ctx, relID)
}

// PersistAuthFiles stores the provided auth file changes in PostgreSQL.
func (s *PostgresStore) PersistAuthFiles(ctx context.Context, _ string, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, p := range paths {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		relID, err := s.relativeAuthID(trimmed)
		if err != nil {
			// Attempt to resolve absolute path under authDir.
			abs := trimmed
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(s.authDir, trimmed)
			}
			relID, err = s.relativeAuthID(abs)
			if err != nil {
				log.WithError(err).Warnf("postgres store: ignoring auth path %s", trimmed)
				continue
			}
			trimmed = abs
		}
		if err = s.syncAuthFile(ctx, relID, trimmed); err != nil {
			return err
		}
	}
	return nil
}

// PersistConfig mirrors the local configuration file to PostgreSQL.
func (s *PostgresStore) PersistConfig(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s.deleteConfigRecord(ctx)
		}
		return fmt.Errorf("postgres store: read config file: %w", err)
	}
	return s.persistConfig(ctx, data)
}

// syncConfigFromDatabase writes the database-stored config to disk or seeds the database from template.
func (s *PostgresStore) syncConfigFromDatabase(ctx context.Context, exampleConfigPath string) error {
	query := fmt.Sprintf("SELECT content FROM %s WHERE id = $1", s.fullTableName(s.cfg.ConfigTable))
	var content string
	err := s.db.QueryRowContext(ctx, query, defaultConfigKey).Scan(&content)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, errStat := os.Stat(s.configPath); errors.Is(errStat, fs.ErrNotExist) {
			if exampleConfigPath != "" {
				if errCopy := misc.CopyConfigTemplate(exampleConfigPath, s.configPath); errCopy != nil {
					return fmt.Errorf("postgres store: copy example config: %w", errCopy)
				}
			} else {
				if errCreate := os.MkdirAll(filepath.Dir(s.configPath), 0o700); errCreate != nil {
					return fmt.Errorf("postgres store: prepare config directory: %w", errCreate)
				}
				if errWrite := os.WriteFile(s.configPath, []byte{}, 0o600); errWrite != nil {
					return fmt.Errorf("postgres store: create empty config: %w", errWrite)
				}
			}
		}
		data, errRead := os.ReadFile(s.configPath)
		if errRead != nil {
			return fmt.Errorf("postgres store: read local config: %w", errRead)
		}
		if errPersist := s.persistConfig(ctx, data); errPersist != nil {
			return errPersist
		}
	case err != nil:
		return fmt.Errorf("postgres store: load config from database: %w", err)
	default:
		if err = os.MkdirAll(filepath.Dir(s.configPath), 0o700); err != nil {
			return fmt.Errorf("postgres store: prepare config directory: %w", err)
		}
		normalized := normalizeLineEndings(content)
		if err = os.WriteFile(s.configPath, []byte(normalized), 0o600); err != nil {
			return fmt.Errorf("postgres store: write config to spool: %w", err)
		}
	}
	return nil
}

// syncAuthFromDatabase populates the local auth directory from PostgreSQL data.
func (s *PostgresStore) syncAuthFromDatabase(ctx context.Context) error {
	query := fmt.Sprintf("SELECT id, content FROM %s WHERE node_ip = $1", s.fullTableName(s.cfg.AuthTable))
	rows, err := s.db.QueryContext(ctx, query, s.cfg.NodeIP)
	if err != nil {
		return fmt.Errorf("postgres store: load auth from database: %w", err)
	}
	defer rows.Close()

	if err = os.RemoveAll(s.authDir); err != nil {
		return fmt.Errorf("postgres store: reset auth directory: %w", err)
	}
	if err = os.MkdirAll(s.authDir, 0o700); err != nil {
		return fmt.Errorf("postgres store: recreate auth directory: %w", err)
	}

	for rows.Next() {
		var (
			id      string
			payload string
		)
		if err = rows.Scan(&id, &payload); err != nil {
			return fmt.Errorf("postgres store: scan auth row: %w", err)
		}
		path, errPath := s.absoluteAuthPath(id)
		if errPath != nil {
			log.WithError(errPath).Warnf("postgres store: skipping auth %s outside spool", id)
			continue
		}
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("postgres store: create auth subdir: %w", err)
		}
		if err = os.WriteFile(path, []byte(payload), 0o600); err != nil {
			return fmt.Errorf("postgres store: write auth file: %w", err)
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("postgres store: iterate auth rows: %w", err)
	}
	return nil
}

func (s *PostgresStore) syncAuthFile(ctx context.Context, relID, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s.deleteAuthRecord(ctx, relID)
		}
		return fmt.Errorf("postgres store: read auth file: %w", err)
	}
	if len(data) == 0 {
		return s.deleteAuthRecord(ctx, relID)
	}
	return s.persistAuth(ctx, relID, data)
}

func (s *PostgresStore) upsertAuthRecord(ctx context.Context, relID, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("postgres store: read auth file: %w", err)
	}
	if len(data) == 0 {
		return s.deleteAuthRecord(ctx, relID)
	}
	return s.persistAuth(ctx, relID, data)
}

func (s *PostgresStore) persistAuth(ctx context.Context, relID string, data []byte) error {
	jsonPayload := json.RawMessage(data)
	query := fmt.Sprintf(`
		INSERT INTO %s (id, content, node_ip, created_at, updated_at)
		VALUES ($1, $2, $3, NOW(), NOW())
		ON CONFLICT (id, node_ip)
		DO UPDATE SET content = EXCLUDED.content, updated_at = NOW()
	`, s.fullTableName(s.cfg.AuthTable))
	if _, err := s.db.ExecContext(ctx, query, relID, jsonPayload, s.cfg.NodeIP); err != nil {
		return fmt.Errorf("postgres store: upsert auth record: %w", err)
	}
	return nil
}

func (s *PostgresStore) deleteAuthRecord(ctx context.Context, relID string) error {
	query := fmt.Sprintf("DELETE FROM %s WHERE id = $1 AND node_ip = $2", s.fullTableName(s.cfg.AuthTable))
	if _, err := s.db.ExecContext(ctx, query, relID, s.cfg.NodeIP); err != nil {
		return fmt.Errorf("postgres store: delete auth record: %w", err)
	}
	return nil
}

func (s *PostgresStore) persistConfig(ctx context.Context, data []byte) error {
	query := fmt.Sprintf(`
		INSERT INTO %s (id, content, created_at, updated_at)
		VALUES ($1, $2, NOW(), NOW())
		ON CONFLICT (id)
		DO UPDATE SET content = EXCLUDED.content, updated_at = NOW()
	`, s.fullTableName(s.cfg.ConfigTable))
	normalized := normalizeLineEndings(string(data))
	if _, err := s.db.ExecContext(ctx, query, defaultConfigKey, normalized); err != nil {
		return fmt.Errorf("postgres store: upsert config: %w", err)
	}
	return nil
}

func (s *PostgresStore) deleteConfigRecord(ctx context.Context) error {
	query := fmt.Sprintf("DELETE FROM %s WHERE id = $1", s.fullTableName(s.cfg.ConfigTable))
	if _, err := s.db.ExecContext(ctx, query, defaultConfigKey); err != nil {
		return fmt.Errorf("postgres store: delete config: %w", err)
	}
	return nil
}

func (s *PostgresStore) resolveAuthPath(auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("postgres store: auth is nil")
	}
	if auth.Attributes != nil {
		if p := strings.TrimSpace(auth.Attributes["path"]); p != "" {
			return p, nil
		}
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		if filepath.IsAbs(fileName) {
			return fileName, nil
		}
		return filepath.Join(s.authDir, fileName), nil
	}
	if auth.ID == "" {
		return "", fmt.Errorf("postgres store: missing id")
	}
	if filepath.IsAbs(auth.ID) {
		return auth.ID, nil
	}
	return filepath.Join(s.authDir, filepath.FromSlash(auth.ID)), nil
}

func (s *PostgresStore) resolveDeletePath(id string) (string, error) {
	if strings.ContainsRune(id, os.PathSeparator) || filepath.IsAbs(id) {
		return id, nil
	}
	return filepath.Join(s.authDir, filepath.FromSlash(id)), nil
}

func (s *PostgresStore) relativeAuthID(path string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("postgres store: store not initialized")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.authDir, path)
	}
	clean := filepath.Clean(path)
	rel, err := filepath.Rel(s.authDir, clean)
	if err != nil {
		return "", fmt.Errorf("postgres store: compute relative path: %w", err)
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("postgres store: path %s outside managed directory", path)
	}
	return filepath.ToSlash(rel), nil
}

func (s *PostgresStore) absoluteAuthPath(id string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("postgres store: store not initialized")
	}
	clean := filepath.Clean(filepath.FromSlash(id))
	if strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("postgres store: invalid auth identifier %s", id)
	}
	path := filepath.Join(s.authDir, clean)
	rel, err := filepath.Rel(s.authDir, path)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("postgres store: resolved auth path escapes auth directory")
	}
	return path, nil
}

func (s *PostgresStore) fullTableName(name string) string {
	if strings.TrimSpace(s.cfg.Schema) == "" {
		return quoteIdentifier(name)
	}
	return quoteIdentifier(s.cfg.Schema) + "." + quoteIdentifier(name)
}

func quoteIdentifier(identifier string) string {
	replaced := strings.ReplaceAll(identifier, "\"", "\"\"")
	return "\"" + replaced + "\""
}

func valueAsString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return ""
	}
}

func labelFor(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	if v := strings.TrimSpace(valueAsString(metadata["label"])); v != "" {
		return v
	}
	if v := strings.TrimSpace(valueAsString(metadata["email"])); v != "" {
		return v
	}
	if v := strings.TrimSpace(valueAsString(metadata["project_id"])); v != "" {
		return v
	}
	return ""
}

func normalizeAuthID(id string) string {
	return filepath.ToSlash(filepath.Clean(id))
}

func normalizeLineEndings(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

// ─── APIKeyRecord ─────────────────────────────────────────────────────────────

// APIKeyRecord represents a row in the api_keys table.
type APIKeyRecord struct {
	Key          string
	Label        string
	QuotaMillion float64
	Disabled     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ListAPIKeys returns all non-disabled api_key records from the database.
func (s *PostgresStore) ListAPIKeys(ctx context.Context) ([]APIKeyRecord, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("postgres store: not initialized")
	}
	query := fmt.Sprintf(
		`SELECT key, label, quota_millions, disabled, created_at, updated_at FROM %s ORDER BY created_at`,
		s.fullTableName(defaultAPIKeysTable),
	)
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("postgres store: list api keys: %w", err)
	}
	defer rows.Close()
	var records []APIKeyRecord
	for rows.Next() {
		var r APIKeyRecord
		if err = rows.Scan(&r.Key, &r.Label, &r.QuotaMillion, &r.Disabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("postgres store: scan api key row: %w", err)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// SaveAPIKey upserts an api_key record.
func (s *PostgresStore) SaveAPIKey(ctx context.Context, r APIKeyRecord) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres store: not initialized")
	}
	query := fmt.Sprintf(`
		INSERT INTO %s (key, label, quota_millions, disabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW())
		ON CONFLICT (key)
		DO UPDATE SET label = EXCLUDED.label,
		              quota_millions = EXCLUDED.quota_millions,
		              disabled = EXCLUDED.disabled,
		              updated_at = NOW()
	`, s.fullTableName(defaultAPIKeysTable))
	if _, err := s.db.ExecContext(ctx, query, r.Key, r.Label, r.QuotaMillion, r.Disabled); err != nil {
		return fmt.Errorf("postgres store: save api key: %w", err)
	}
	return nil
}

// DeleteAPIKey removes an api_key record.
func (s *PostgresStore) DeleteAPIKey(ctx context.Context, key string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres store: not initialized")
	}
	query := fmt.Sprintf("DELETE FROM %s WHERE key = $1", s.fullTableName(defaultAPIKeysTable))
	if _, err := s.db.ExecContext(ctx, query, key); err != nil {
		return fmt.Errorf("postgres store: delete api key: %w", err)
	}
	return nil
}

// ─── UsageRecord ──────────────────────────────────────────────────────────────

// UsageRecord represents a row in the usage_records table.
type UsageRecord struct {
	APIKey          string
	NodeIP          string
	Provider        string
	Model           string
	AuthID          string
	Source          string
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CachedTokens    int64
	TotalTokens     int64
	Failed          bool
	RequestedAt     time.Time
}

// InsertUsageRecord enqueues a usage record for asynchronous batch insertion.
// The method is non-blocking; records are flushed every 5 seconds or when 100 are accumulated.
func (s *PostgresStore) InsertUsageRecord(_ context.Context, r UsageRecord) {
	if s == nil || s.usageCh == nil {
		return
	}
	select {
	case s.usageCh <- r:
	default:
		// channel full, drop to avoid blocking the request path
	}
}

// startUsageWorker launches a background goroutine that batches usage_records writes.
func (s *PostgresStore) startUsageWorker() {
	s.usageCh = make(chan UsageRecord, 500)
	s.usageStop = make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		var batch []UsageRecord
		flush := func() {
			if len(batch) == 0 {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := s.flushUsageBatch(ctx, batch); err != nil {
				log.WithError(err).Warn("postgres store: flush usage batch failed")
			}
			batch = batch[:0]
		}
		for {
			select {
			case r := <-s.usageCh:
				batch = append(batch, r)
				if len(batch) >= 100 {
					flush()
				}
			case <-ticker.C:
				flush()
			case <-s.usageStop:
				// drain remaining
				for {
					select {
					case r := <-s.usageCh:
						batch = append(batch, r)
					default:
						flush()
						return
					}
				}
			}
		}
	}()
}

func (s *PostgresStore) flushUsageBatch(ctx context.Context, batch []UsageRecord) error {
	if len(batch) == 0 {
		return nil
	}
	table := s.fullTableName(defaultUsageTable)
	// Build a multi-row INSERT
	valueArgs := make([]any, 0, len(batch)*14)
	placeholders := make([]string, 0, len(batch))
	for i, r := range batch {
		at := r.RequestedAt
		if at.IsZero() {
			at = time.Now()
		}
		base := i * 13
		placeholders = append(placeholders, fmt.Sprintf(
			"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6,
			base+7, base+8, base+9, base+10, base+11, base+12, base+13,
		))
		valueArgs = append(valueArgs,
			r.APIKey, r.NodeIP, r.Provider, r.Model, r.AuthID, r.Source,
			r.InputTokens, r.OutputTokens, r.ReasoningTokens, r.CachedTokens, r.TotalTokens,
			r.Failed, at,
		)
	}
	query := fmt.Sprintf(
		`INSERT INTO %s (api_key,node_ip,provider,model,auth_id,source,input_tokens,output_tokens,reasoning_tokens,cached_tokens,total_tokens,failed,requested_at) VALUES %s`,
		table, strings.Join(placeholders, ","),
	)
	_, err := s.db.ExecContext(ctx, query, valueArgs...)
	return err
}

// QueryUsageAggregate executes a configurable aggregate query over usage_records.
func (s *PostgresStore) QueryUsageAggregate(ctx context.Context, params UsageAggregateParams) ([]UsageAggregateRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("postgres store: not initialized")
	}
	groupCol := "api_key"
	switch params.GroupBy {
	case "node_ip":
		groupCol = "node_ip"
	case "model":
		groupCol = "model"
	case "day":
		groupCol = "DATE_TRUNC('day', requested_at)"
	}

	where := []string{"1=1"}
	args := []any{}
	argIdx := 1
	if params.APIKey != "" {
		where = append(where, fmt.Sprintf("api_key = $%d", argIdx))
		args = append(args, params.APIKey)
		argIdx++
	}
	if params.NodeIP != "" {
		where = append(where, fmt.Sprintf("node_ip = $%d", argIdx))
		args = append(args, params.NodeIP)
		argIdx++
	}
	if !params.From.IsZero() {
		where = append(where, fmt.Sprintf("requested_at >= $%d", argIdx))
		args = append(args, params.From)
		argIdx++
	}
	if !params.To.IsZero() {
		where = append(where, fmt.Sprintf("requested_at <= $%d", argIdx))
		args = append(args, params.To)
		argIdx++
	}

	query := fmt.Sprintf(`
		SELECT %s AS group_key,
		       COUNT(*) AS requests,
		       SUM(input_tokens) AS input_tokens,
		       SUM(output_tokens) AS output_tokens,
		       SUM(total_tokens) AS total_tokens,
		       SUM(CASE WHEN failed THEN 1 ELSE 0 END) AS failed_count
		FROM %s
		WHERE %s
		GROUP BY %s
		ORDER BY requests DESC
	`, groupCol, s.fullTableName(defaultUsageTable), strings.Join(where, " AND "), groupCol)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres store: usage aggregate query: %w", err)
	}
	defer rows.Close()
	var result []UsageAggregateRow
	for rows.Next() {
		var row UsageAggregateRow
		if err = rows.Scan(&row.GroupKey, &row.Requests, &row.InputTokens, &row.OutputTokens, &row.TotalTokens, &row.FailedCount); err != nil {
			return nil, fmt.Errorf("postgres store: scan aggregate row: %w", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// UsageAggregateParams specifies filters for QueryUsageAggregate.
type UsageAggregateParams struct {
	APIKey  string
	NodeIP  string
	From    time.Time
	To      time.Time
	GroupBy string // api_key | node_ip | model | day
}

// UsageAggregateRow is one result row from QueryUsageAggregate.
type UsageAggregateRow struct {
	GroupKey     string `json:"group_key"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	FailedCount  int64  `json:"failed_count"`
}

// ─── DetectLocalIP ────────────────────────────────────────────────────────────

// DetectLocalIP returns the local outbound IP address by connecting (without sending data)
// to a well-known external address. Returns empty string on failure.
func DetectLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	return localAddr.IP.String()
}
