package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
	cliproxyauth "proxycore/api/v6/sdk/cliproxy/auth"
)

const (
	defaultConfigTable        = "config_store"
	defaultAuthTable          = "auth_store"
	defaultConfigKey          = "config"
	defaultAPIKeysTable       = "api_keys"
	defaultUsageTable         = "usage_records"
	defaultNodesTable         = "nodes"
	defaultProxiesTable       = "proxies"
	defaultOAuthCallbackTable = "oauth_callbacks"
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

// authCooldownState is stored in the cooldown_state JSONB column so that
// cooldown/quota state survives service restarts.
type authCooldownState struct {
	NextRetryAfter time.Time                           `json:"next_retry_after,omitempty"`
	Unavailable    bool                                `json:"unavailable,omitempty"`
	StatusMessage  string                              `json:"status_message,omitempty"`
	ModelStates    map[string]*cliproxyauth.ModelState `json:"model_states,omitempty"`
}

// authDirtyItem queues an async cooldown-state write.
type authDirtyItem struct {
	id       string
	cooldown authCooldownState
}

type OAuthCallbackRecord struct {
	Provider  string
	State     string
	Code      string
	Error     string
	CreatedAt time.Time
}

// PostgresStore persists configuration and authentication metadata using PostgreSQL as backend.
// All config and auth operations go directly to the database; no local file mirroring is performed.
type PostgresStore struct {
	db            *sql.DB
	cfg           PostgresStoreConfig
	configContent string // in-memory cache of the YAML config
	mu            sync.Mutex

	// usage async writer
	usageCh   chan UsageRecord
	usageStop chan struct{}

	// async cooldown-state writer (updates cooldown_state column only)
	authDirtyCh chan authDirtyItem

	// contentHashes caches the SHA-256 of the last persisted content per record ID.
	// Save() skips the sync DB UPSERT when the token hasn't changed (MarkResult path).
	contentHashes sync.Map // map[string]string: recID → hex(sha256)
}

func persistedAuthMetadata(auth *cliproxyauth.Auth) map[string]any {
	if auth == nil {
		return nil
	}
	meta := make(map[string]any, len(auth.Metadata)+3)
	for key, value := range auth.Metadata {
		meta[key] = value
	}
	meta["disabled"] = auth.Disabled
	if prefix := strings.TrimSpace(auth.Prefix); prefix != "" {
		meta["prefix"] = prefix
	} else {
		delete(meta, "prefix")
	}
	if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
		meta["proxy_url"] = proxyURL
	} else {
		delete(meta, "proxy_url")
	}
	return meta
}

func disabledFromMetadata(metadata map[string]any) bool {
	disabled, _ := metadata["disabled"].(bool)
	return disabled
}

func statusFromDisabled(metadata map[string]any) cliproxyauth.Status {
	if disabledFromMetadata(metadata) {
		return cliproxyauth.StatusDisabled
	}
	return cliproxyauth.StatusActive
}

// NewPostgresStore establishes a connection to PostgreSQL.
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

	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres store: open database connection: %w", err)
	}
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres store: ping database: %w", err)
	}

	store := &PostgresStore{
		db:  db,
		cfg: cfg,
	}
	store.startUsageWorker()
	store.startAuthDirtyWorker()
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
		`ALTER TABLE %s ADD COLUMN IF NOT EXISTS cooldown_state JSONB`,
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
	// Migrate existing usage_records tables that may be missing newer columns.
	for _, col := range []struct{ name, def string }{
		{"node_ip", "TEXT NOT NULL DEFAULT ''"},
		{"auth_id", "TEXT NOT NULL DEFAULT ''"},
		{"source", "TEXT NOT NULL DEFAULT ''"},
		{"reasoning_tokens", "BIGINT NOT NULL DEFAULT 0"},
		{"cached_tokens", "BIGINT NOT NULL DEFAULT 0"},
	} {
		_, _ = s.db.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s`,
			usageTable, col.name, col.def,
		))
	}

	// nodes registry table — each node upserts itself on startup
	nodesTable := s.fullTableName(defaultNodesTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			node_ip    TEXT PRIMARY KEY,
			registered_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`, nodesTable)); err != nil {
		return fmt.Errorf("postgres store: create nodes table: %w", err)
	}
	// Register this node
	if s.cfg.NodeIP != "" {
		_, _ = s.db.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO %s (node_ip, registered_at, last_seen_at)
			VALUES ($1, NOW(), NOW())
			ON CONFLICT (node_ip) DO UPDATE SET last_seen_at = NOW()
		`, nodesTable), s.cfg.NodeIP)
	}

	proxiesTable := s.fullTableName(defaultProxiesTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			name TEXT PRIMARY KEY,
			proxy_url TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`, proxiesTable)); err != nil {
		return fmt.Errorf("postgres store: create proxies table: %w", err)
	}

	oauthCallbackTable := s.fullTableName(defaultOAuthCallbackTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			provider TEXT NOT NULL,
			state TEXT NOT NULL,
			code TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (provider, state)
		)
	`, oauthCallbackTable)); err != nil {
		return fmt.Errorf("postgres store: create oauth callbacks table: %w", err)
	}
	_, _ = s.db.ExecContext(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS idx_oauth_callbacks_created_at ON %s (created_at)`,
		oauthCallbackTable,
	))

	return nil
}

// Bootstrap loads configuration from PostgreSQL (or seeds it from template) and ensures defaults.
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
	if err := s.ensureDefaultManagementPassword(ctx); err != nil {
		log.WithError(err).Warn("failed to ensure default management password")
	}
	return nil
}

const managementPasswordKey = "management_password"

// GetManagementPasswordHash returns the bcrypt hash of the management password stored in the DB.
// Returns empty string if not set.
func (s *PostgresStore) GetManagementPasswordHash(ctx context.Context) (string, error) {
	query := fmt.Sprintf("SELECT content FROM %s WHERE id = $1", s.fullTableName(s.cfg.ConfigTable))
	var hash string
	err := s.db.QueryRowContext(ctx, query, managementPasswordKey).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return hash, err
}

// SetManagementPasswordHash persists a bcrypt hash as the management password in the DB.
func (s *PostgresStore) SetManagementPasswordHash(ctx context.Context, hash string) error {
	table := s.fullTableName(s.cfg.ConfigTable)
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, content, created_at, updated_at)
		VALUES ($1, $2, NOW(), NOW())
		ON CONFLICT (id) DO UPDATE SET content = EXCLUDED.content, updated_at = NOW()
	`, table), managementPasswordKey, hash)
	return err
}

// ensureDefaultManagementPassword sets the management password to "admin" (bcrypt hashed) if unset.
func (s *PostgresStore) ensureDefaultManagementPassword(ctx context.Context) error {
	existing, err := s.GetManagementPasswordHash(ctx)
	if err != nil {
		return err
	}
	if existing != "" {
		return nil
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("postgres store: hash default management password: %w", err)
	}
	if err := s.SetManagementPasswordHash(ctx, string(hashed)); err != nil {
		return fmt.Errorf("postgres store: persist default management password: %w", err)
	}
	log.Info("postgres store: default management password set to 'admin'")
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
	s.configContent = updated
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

// ConfigPath returns empty string; PG mode has no spool config file.
func (s *PostgresStore) ConfigPath() string {
	return ""
}

// AuthDir returns empty string; PG mode has no local auth directory.
func (s *PostgresStore) AuthDir() string {
	return ""
}

// NodeIP returns the node IP configured for this store instance.
func (s *PostgresStore) NodeIP() string {
	if s == nil {
		return ""
	}
	return s.cfg.NodeIP
}

// WorkDir returns empty string; PG mode has no spool directory.
func (s *PostgresStore) WorkDir() string {
	return ""
}

// SetBaseDir implements the optional interface used by authenticators; it is a no-op because
// the Postgres-backed store controls its own workspace.
func (s *PostgresStore) SetBaseDir(string) {}

// Save persists authentication metadata to disk and PostgreSQL.
func (s *PostgresStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("postgres store: auth is nil")
	}

	// Derive a stable record ID from auth.ID or FileName.
	recID := strings.TrimSpace(auth.ID)
	if recID == "" {
		recID = strings.TrimSpace(auth.FileName)
	}
	if recID == "" {
		return "", fmt.Errorf("postgres store: auth has no id or filename")
	}
	recID = filepath.ToSlash(filepath.Base(recID))

	var raw []byte
	var err error
	switch {
	case auth.Storage != nil:
		type metadataSetter interface {
			SetMetadata(map[string]any)
		}
		if setter, ok := auth.Storage.(metadataSetter); ok {
			setter.SetMetadata(persistedAuthMetadata(auth))
		}
		// Use a temp file to serialize the token storage, then read it back.
		tmp, errTmp := os.CreateTemp("", "pgstore-auth-*.json")
		if errTmp != nil {
			return "", fmt.Errorf("postgres store: create temp file: %w", errTmp)
		}
		tmpPath := tmp.Name()
		tmp.Close()
		defer os.Remove(tmpPath)
		if err = auth.Storage.SaveTokenToFile(tmpPath); err != nil {
			return "", fmt.Errorf("postgres store: serialize storage token: %w", err)
		}
		raw, err = os.ReadFile(tmpPath)
		if err != nil {
			return "", fmt.Errorf("postgres store: read serialized token: %w", err)
		}
	case auth.Metadata != nil:
		auth.Metadata = persistedAuthMetadata(auth)
		raw, err = json.Marshal(auth.Metadata)
		if err != nil {
			return "", fmt.Errorf("postgres store: marshal metadata: %w", err)
		}
	default:
		return "", fmt.Errorf("postgres store: nothing to persist for %s", recID)
	}

	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = recID
	}

	// Skip the sync UPSERT when the token content hasn't changed (e.g. MarkResult path).
	h := sha256.Sum256(raw)
	hashStr := hex.EncodeToString(h[:])
	if prev, loaded := s.contentHashes.Load(recID); loaded && prev.(string) == hashStr {
		// Content unchanged — only enqueue async cooldown update.
		s.enqueueCooldown(recID, auth)
		return recID, nil
	}
	if err = s.persistAuth(ctx, recID, raw); err != nil {
		return "", err
	}
	s.contentHashes.Store(recID, hashStr)

	// Enqueue cooldown state asynchronously (non-blocking).
	s.enqueueCooldown(recID, auth)
	return recID, nil
}

// List enumerates all auth records stored in PostgreSQL for this node.
func (s *PostgresStore) List(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	query := fmt.Sprintf("SELECT id, content, cooldown_state, created_at, updated_at FROM %s WHERE node_ip = $1 ORDER BY id", s.fullTableName(s.cfg.AuthTable))
	rows, err := s.db.QueryContext(ctx, query, s.cfg.NodeIP)
	if err != nil {
		return nil, fmt.Errorf("postgres store: list auth: %w", err)
	}
	defer rows.Close()

	now := time.Now()
	auths := make([]*cliproxyauth.Auth, 0, 32)
	for rows.Next() {
		var (
			id           string
			payload      string
			cooldownJSON []byte
			createdAt    time.Time
			updatedAt    time.Time
		)
		if err = rows.Scan(&id, &payload, &cooldownJSON, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("postgres store: scan auth row: %w", err)
		}
		path := normalizeAuthID(id)
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
		disabled := disabledFromMetadata(metadata)
		auth := &cliproxyauth.Auth{
			ID:               normalizeAuthID(id),
			Provider:         provider,
			FileName:         normalizeAuthID(id),
			Label:            labelFor(metadata),
			Status:           statusFromDisabled(metadata),
			Disabled:         disabled,
			Prefix:           strings.TrimSpace(valueAsString(metadata["prefix"])),
			ProxyURL:         strings.TrimSpace(valueAsString(metadata["proxy_url"])),
			Attributes:       attr,
			Metadata:         metadata,
			CreatedAt:        createdAt,
			UpdatedAt:        updatedAt,
			LastRefreshedAt:  time.Time{},
			NextRefreshAfter: time.Time{},
		}

		restoreCooldownState(auth, cooldownJSON, now)

		auths = append(auths, auth)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres store: iterate auth rows: %w", err)
	}
	return auths, nil
}

// Delete removes the auth record from the database.
func (s *PostgresStore) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("postgres store: id is empty")
	}
	recID := filepath.ToSlash(filepath.Base(id))
	return s.deleteAuthRecord(ctx, recID)
}

// DeleteAuthByID removes an auth record by ID only, without node_ip constraint.
func (s *PostgresStore) DeleteAuthByID(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("postgres store: id is empty")
	}
	query := fmt.Sprintf("DELETE FROM %s WHERE id = $1", s.fullTableName(s.cfg.AuthTable))
	if _, err := s.db.ExecContext(ctx, query, id); err != nil {
		return fmt.Errorf("postgres store: delete auth by id: %w", err)
	}
	return nil
}

// DeleteAllAuth removes all auth records from the database.
func (s *PostgresStore) DeleteAllAuth(ctx context.Context) error {
	query := fmt.Sprintf("DELETE FROM %s", s.fullTableName(s.cfg.AuthTable))
	if _, err := s.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("postgres store: delete all auth: %w", err)
	}
	return nil
}

// ListNodes returns all registered node IPs from the nodes registry table.
func (s *PostgresStore) ListNodes(ctx context.Context) ([]string, error) {
	query := fmt.Sprintf("SELECT node_ip FROM %s ORDER BY node_ip", s.fullTableName(defaultNodesTable))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("postgres store: list nodes: %w", err)
	}
	defer rows.Close()
	var nodes []string
	for rows.Next() {
		var ip string
		if err = rows.Scan(&ip); err != nil {
			return nil, fmt.Errorf("postgres store: scan node ip: %w", err)
		}
		nodes = append(nodes, ip)
	}
	return nodes, rows.Err()
}

// NodeRecord represents a row in the nodes table.
type NodeRecord struct {
	NodeIP       string
	RegisteredAt time.Time
	LastSeenAt   time.Time
}

type ProxyRecord struct {
	Name        string
	ProxyURL    string
	Description string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ListNodeRecords returns node registry entries with timestamps.
func (s *PostgresStore) ListNodeRecords(ctx context.Context) ([]NodeRecord, error) {
	query := fmt.Sprintf(
		"SELECT node_ip, registered_at, last_seen_at FROM %s ORDER BY node_ip",
		s.fullTableName(defaultNodesTable),
	)
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("postgres store: list node records: %w", err)
	}
	defer rows.Close()
	var records []NodeRecord
	for rows.Next() {
		var r NodeRecord
		if err = rows.Scan(&r.NodeIP, &r.RegisteredAt, &r.LastSeenAt); err != nil {
			return nil, fmt.Errorf("postgres store: scan node record: %w", err)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// SaveNode inserts or updates a node registry entry.
func (s *PostgresStore) SaveNode(ctx context.Context, r NodeRecord) error {
	nodeIP := strings.TrimSpace(r.NodeIP)
	if nodeIP == "" {
		return fmt.Errorf("postgres store: node_ip is required")
	}
	registeredAt := r.RegisteredAt
	if registeredAt.IsZero() {
		registeredAt = time.Now().UTC()
	}
	lastSeenAt := r.LastSeenAt
	if lastSeenAt.IsZero() {
		lastSeenAt = registeredAt
	}
	query := fmt.Sprintf(`
		INSERT INTO %s (node_ip, registered_at, last_seen_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (node_ip) DO UPDATE
		SET registered_at = EXCLUDED.registered_at,
		    last_seen_at = EXCLUDED.last_seen_at
	`, s.fullTableName(defaultNodesTable))
	if _, err := s.db.ExecContext(ctx, query, nodeIP, registeredAt, lastSeenAt); err != nil {
		return fmt.Errorf("postgres store: save node: %w", err)
	}
	return nil
}

// RenameNode updates node_ip across node, auth, and usage tables.
func (s *PostgresStore) RenameNode(ctx context.Context, oldNodeIP, newNodeIP string) error {
	oldNodeIP = strings.TrimSpace(oldNodeIP)
	newNodeIP = strings.TrimSpace(newNodeIP)
	if oldNodeIP == "" || newNodeIP == "" {
		return fmt.Errorf("postgres store: old and new node_ip are required")
	}
	if oldNodeIP == newNodeIP {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres store: begin rename node tx: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	res, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET node_ip = $2 WHERE node_ip = $1`, s.fullTableName(defaultNodesTable)),
		oldNodeIP, newNodeIP,
	)
	if err != nil {
		return fmt.Errorf("postgres store: rename node registry record: %w", err)
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres store: read rename node result: %w", err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("postgres store: node not found")
	}

	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET node_ip = $2 WHERE node_ip = $1`, s.fullTableName(s.cfg.AuthTable)),
		oldNodeIP, newNodeIP,
	); err != nil {
		return fmt.Errorf("postgres store: rename node auth records: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET node_ip = $2 WHERE node_ip = $1`, s.fullTableName(defaultUsageTable)),
		oldNodeIP, newNodeIP,
	); err != nil {
		return fmt.Errorf("postgres store: rename node usage records: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres store: commit rename node tx: %w", err)
	}
	tx = nil
	return nil
}

// DeleteNode removes a node registry entry and all node-scoped auth/usage data.
func (s *PostgresStore) DeleteNode(ctx context.Context, nodeIP string) error {
	nodeIP = strings.TrimSpace(nodeIP)
	if nodeIP == "" {
		return fmt.Errorf("postgres store: node_ip is required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres store: begin delete node tx: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE node_ip = $1`, s.fullTableName(s.cfg.AuthTable)), nodeIP); err != nil {
		return fmt.Errorf("postgres store: delete node auth records: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE node_ip = $1`, s.fullTableName(defaultUsageTable)), nodeIP); err != nil {
		return fmt.Errorf("postgres store: delete node usage records: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE node_ip = $1`, s.fullTableName(defaultNodesTable)), nodeIP); err != nil {
		return fmt.Errorf("postgres store: delete node registry record: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres store: commit delete node tx: %w", err)
	}
	tx = nil
	return nil
}

// ListAuthByNode enumerates all auth records for a specific node_ip.
func (s *PostgresStore) ListAuthByNode(ctx context.Context, nodeIP string) ([]*cliproxyauth.Auth, error) {
	query := fmt.Sprintf("SELECT id, content, cooldown_state, created_at, updated_at FROM %s WHERE node_ip = $1 ORDER BY id", s.fullTableName(s.cfg.AuthTable))
	rows, err := s.db.QueryContext(ctx, query, nodeIP)
	if err != nil {
		return nil, fmt.Errorf("postgres store: list auth by node: %w", err)
	}
	defer rows.Close()

	auths := make([]*cliproxyauth.Auth, 0, 32)
	now := time.Now()
	for rows.Next() {
		var (
			id           string
			payload      string
			cooldownJSON []byte
			createdAt    time.Time
			updatedAt    time.Time
		)
		if err = rows.Scan(&id, &payload, &cooldownJSON, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("postgres store: scan auth row: %w", err)
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
		attr := map[string]string{"runtime_only": "true"}
		if email := strings.TrimSpace(valueAsString(metadata["email"])); email != "" {
			attr["email"] = email
		}
		disabled := disabledFromMetadata(metadata)
		auth := &cliproxyauth.Auth{
			ID:         normalizeAuthID(id),
			Provider:   provider,
			FileName:   normalizeAuthID(id),
			Label:      labelFor(metadata),
			Status:     statusFromDisabled(metadata),
			Disabled:   disabled,
			Prefix:     strings.TrimSpace(valueAsString(metadata["prefix"])),
			ProxyURL:   strings.TrimSpace(valueAsString(metadata["proxy_url"])),
			Attributes: attr,
			Metadata:   metadata,
			CreatedAt:  createdAt,
			UpdatedAt:  updatedAt,
		}
		restoreCooldownState(auth, cooldownJSON, now)
		auths = append(auths, auth)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres store: iterate auth rows: %w", err)
	}
	return auths, nil
}

// ListAllAuth returns all auth records across all nodes.
func (s *PostgresStore) ListAllAuth(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	query := fmt.Sprintf("SELECT id, content, cooldown_state, created_at, updated_at FROM %s ORDER BY id", s.fullTableName(s.cfg.AuthTable))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("postgres store: list all auth: %w", err)
	}
	defer rows.Close()

	auths := make([]*cliproxyauth.Auth, 0, 32)
	now := time.Now()
	for rows.Next() {
		var (
			id           string
			payload      string
			cooldownJSON []byte
			createdAt    time.Time
			updatedAt    time.Time
		)
		if err = rows.Scan(&id, &payload, &cooldownJSON, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("postgres store: scan auth row: %w", err)
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
		attr := map[string]string{"runtime_only": "true"}
		if email := strings.TrimSpace(valueAsString(metadata["email"])); email != "" {
			attr["email"] = email
		}
		disabled := disabledFromMetadata(metadata)
		auth := &cliproxyauth.Auth{
			ID:         normalizeAuthID(id),
			Provider:   provider,
			FileName:   normalizeAuthID(id),
			Label:      labelFor(metadata),
			Status:     statusFromDisabled(metadata),
			Disabled:   disabled,
			Prefix:     strings.TrimSpace(valueAsString(metadata["prefix"])),
			ProxyURL:   strings.TrimSpace(valueAsString(metadata["proxy_url"])),
			Attributes: attr,
			Metadata:   metadata,
			CreatedAt:  createdAt,
			UpdatedAt:  updatedAt,
		}
		restoreCooldownState(auth, cooldownJSON, now)
		auths = append(auths, auth)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres store: iterate auth rows: %w", err)
	}
	return auths, nil
}

// PersistAuthFiles is a no-op in PG mode; auth records are written directly to the DB via Save().
func (s *PostgresStore) PersistAuthFiles(_ context.Context, _ string, _ ...string) error {
	return nil
}

// PersistConfig is a no-op in PG mode; SaveConfigContent writes directly to the DB.
func (s *PostgresStore) PersistConfig(_ context.Context) error {
	return nil
}

// GetConfigContent returns the in-memory cached YAML configuration.
func (s *PostgresStore) GetConfigContent(_ context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.configContent, nil
}

// SaveConfigContent stores content in memory and persists it to the database.
func (s *PostgresStore) SaveConfigContent(ctx context.Context, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persistConfig(ctx, []byte(content)); err != nil {
		return err
	}
	s.configContent = content
	return nil
}

// syncConfigFromDatabase loads config from DB into s.configContent, or seeds DB from template.
func (s *PostgresStore) syncConfigFromDatabase(ctx context.Context, exampleConfigPath string) error {
	query := fmt.Sprintf("SELECT content FROM %s WHERE id = $1", s.fullTableName(s.cfg.ConfigTable))
	var content string
	err := s.db.QueryRowContext(ctx, query, defaultConfigKey).Scan(&content)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var data []byte
		if exampleConfigPath != "" {
			data, err = os.ReadFile(exampleConfigPath)
			if err != nil {
				return fmt.Errorf("postgres store: read example config: %w", err)
			}
		}
		if errPersist := s.persistConfig(ctx, data); errPersist != nil {
			return errPersist
		}
		s.configContent = string(data)
	case err != nil:
		return fmt.Errorf("postgres store: load config from database: %w", err)
	default:
		s.configContent = normalizeLineEndings(content)
	}
	return nil
}

func (s *PostgresStore) persistAuth(ctx context.Context, relID string, data []byte) error {
	return s.persistAuthForNode(ctx, relID, data, s.cfg.NodeIP)
}

func (s *PostgresStore) persistAuthForNode(ctx context.Context, relID string, data []byte, nodeIP string) error {
	jsonPayload := json.RawMessage(data)
	query := fmt.Sprintf(`
		INSERT INTO %s (id, content, node_ip, created_at, updated_at)
		VALUES ($1, $2, $3, NOW(), NOW())
		ON CONFLICT (id, node_ip)
		DO UPDATE SET content = EXCLUDED.content, updated_at = NOW()
	`, s.fullTableName(s.cfg.AuthTable))
	if _, err := s.db.ExecContext(ctx, query, relID, jsonPayload, nodeIP); err != nil {
		return fmt.Errorf("postgres store: upsert auth record: %w", err)
	}
	return nil
}

// SaveForNode persists the auth record under the specified nodeIP instead of this instance's NodeIP.
// Falls back to Save when nodeIP is empty.
func (s *PostgresStore) SaveForNode(ctx context.Context, auth *cliproxyauth.Auth, nodeIP string) (string, error) {
	nodeIP = strings.TrimSpace(nodeIP)
	if nodeIP == "" {
		return s.Save(ctx, auth)
	}
	if auth == nil {
		return "", fmt.Errorf("postgres store: auth is nil")
	}

	recID := strings.TrimSpace(auth.ID)
	if recID == "" {
		recID = strings.TrimSpace(auth.FileName)
	}
	if recID == "" {
		return "", fmt.Errorf("postgres store: auth has no id or filename")
	}
	recID = filepath.ToSlash(filepath.Base(recID))

	var raw []byte
	var err error
	switch {
	case auth.Storage != nil:
		type metadataSetter interface {
			SetMetadata(map[string]any)
		}
		if setter, ok := auth.Storage.(metadataSetter); ok {
			setter.SetMetadata(persistedAuthMetadata(auth))
		}
		tmp, errTmp := os.CreateTemp("", "pgstore-auth-*.json")
		if errTmp != nil {
			return "", fmt.Errorf("postgres store: create temp file: %w", errTmp)
		}
		tmpPath := tmp.Name()
		tmp.Close()
		defer os.Remove(tmpPath)
		if err = auth.Storage.SaveTokenToFile(tmpPath); err != nil {
			return "", fmt.Errorf("postgres store: serialize storage token: %w", err)
		}
		raw, err = os.ReadFile(tmpPath)
		if err != nil {
			return "", fmt.Errorf("postgres store: read serialized token: %w", err)
		}
	case auth.Metadata != nil:
		auth.Metadata = persistedAuthMetadata(auth)
		raw, err = json.Marshal(auth.Metadata)
		if err != nil {
			return "", fmt.Errorf("postgres store: marshal metadata: %w", err)
		}
	default:
		return "", fmt.Errorf("postgres store: nothing to persist for %s", recID)
	}

	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = recID
	}

	if err = s.persistAuthForNode(ctx, recID, raw, nodeIP); err != nil {
		return "", err
	}
	return recID, nil
}

// enqueueCooldown schedules an async write of the auth's cooldown/quota state.
// Non-blocking: if the channel is full the update is silently dropped (the state is
// transient and will be re-written on the next MarkResult cycle).
func (s *PostgresStore) enqueueCooldown(id string, auth *cliproxyauth.Auth) {
	if s.authDirtyCh == nil || auth == nil {
		return
	}
	item := authDirtyItem{
		id: id,
		cooldown: authCooldownState{
			NextRetryAfter: auth.NextRetryAfter,
			Unavailable:    auth.Unavailable,
			StatusMessage:  strings.TrimSpace(auth.StatusMessage),
			ModelStates:    auth.ModelStates,
		},
	}
	select {
	case s.authDirtyCh <- item:
	default: // channel full – drop; next MarkResult will retry
	}
}

func restoreCooldownState(auth *cliproxyauth.Auth, cooldownJSON []byte, now time.Time) {
	if auth == nil || len(cooldownJSON) == 0 {
		return
	}
	var cs authCooldownState
	if err := json.Unmarshal(cooldownJSON, &cs); err != nil {
		return
	}
	if cs.NextRetryAfter.After(now) {
		auth.Unavailable = cs.Unavailable
		auth.NextRetryAfter = cs.NextRetryAfter
		if msg := strings.TrimSpace(cs.StatusMessage); msg != "" {
			auth.StatusMessage = msg
		}
	}
	if len(cs.ModelStates) > 0 && cs.NextRetryAfter.After(now) {
		auth.ModelStates = cs.ModelStates
	}
}

// startAuthDirtyWorker launches the background goroutine that batches cooldown-state writes.
func (s *PostgresStore) startAuthDirtyWorker() {
	s.authDirtyCh = make(chan authDirtyItem, 256)
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		// pending deduplicates by id – keeps only the latest state per auth.
		pending := make(map[string]authDirtyItem)
		flush := func() {
			if len(pending) == 0 {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.flushAuthDirtyBatch(ctx, pending); err != nil {
				log.WithError(err).Warn("postgres store: flush auth cooldown batch failed")
			}
			pending = make(map[string]authDirtyItem)
		}
		for {
			select {
			case item := <-s.authDirtyCh:
				pending[item.id] = item // overwrite → keep latest
			case <-ticker.C:
				flush()
			case <-s.usageStop: // reuse the same stop signal
				for {
					select {
					case item := <-s.authDirtyCh:
						pending[item.id] = item
					default:
						flush()
						return
					}
				}
			}
		}
	}()
}

// flushAuthDirtyBatch writes cooldown_state for each pending auth in one transaction.
func (s *PostgresStore) flushAuthDirtyBatch(ctx context.Context, batch map[string]authDirtyItem) error {
	if len(batch) == 0 {
		return nil
	}
	table := s.fullTableName(s.cfg.AuthTable)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres store: begin cooldown tx: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf(`
		UPDATE %s SET cooldown_state = $1, updated_at = NOW()
		WHERE id = $2 AND node_ip = $3
	`, table))
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("postgres store: prepare cooldown stmt: %w", err)
	}
	defer stmt.Close()
	for _, item := range batch {
		csJSON, errMarshal := json.Marshal(item.cooldown)
		if errMarshal != nil {
			continue
		}
		if _, errExec := stmt.ExecContext(ctx, csJSON, item.id, s.cfg.NodeIP); errExec != nil {
			_ = tx.Rollback()
			return fmt.Errorf("postgres store: update cooldown for %s: %w", item.id, errExec)
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) ListProxies(ctx context.Context) ([]ProxyRecord, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("postgres store: not initialized")
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT name, proxy_url, description, enabled, created_at, updated_at FROM %s ORDER BY LOWER(name), created_at`,
		s.fullTableName(defaultProxiesTable),
	))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []ProxyRecord
	for rows.Next() {
		var r ProxyRecord
		if err = rows.Scan(&r.Name, &r.ProxyURL, &r.Description, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *PostgresStore) SaveProxy(ctx context.Context, r ProxyRecord) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres store: not initialized")
	}
	name := strings.TrimSpace(r.Name)
	proxyURL := strings.TrimSpace(r.ProxyURL)
	if name == "" || proxyURL == "" {
		return fmt.Errorf("postgres store: name and proxy_url are required")
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (name, proxy_url, description, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW())
		ON CONFLICT (name) DO UPDATE
		SET proxy_url = EXCLUDED.proxy_url,
		    description = EXCLUDED.description,
		    enabled = EXCLUDED.enabled,
		    updated_at = NOW()
	`, s.fullTableName(defaultProxiesTable)), name, proxyURL, strings.TrimSpace(r.Description), r.Enabled)
	return err
}

func (s *PostgresStore) UpdateProxy(ctx context.Context, oldName string, r ProxyRecord) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres store: not initialized")
	}
	oldName = strings.TrimSpace(oldName)
	name := strings.TrimSpace(r.Name)
	proxyURL := strings.TrimSpace(r.ProxyURL)
	if oldName == "" || name == "" || proxyURL == "" {
		return fmt.Errorf("postgres store: oldName, name and proxy_url are required")
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %s
		SET name = $2, proxy_url = $3, description = $4, enabled = $5, updated_at = NOW()
		WHERE name = $1
	`, s.fullTableName(defaultProxiesTable)), oldName, name, proxyURL, strings.TrimSpace(r.Description), r.Enabled)
	return err
}

func (s *PostgresStore) DeleteProxy(ctx context.Context, name string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres store: not initialized")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("postgres store: name is required")
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE name = $1`, s.fullTableName(defaultProxiesTable)), name)
	return err
}

func (s *PostgresStore) SaveOAuthCallback(ctx context.Context, r OAuthCallbackRecord) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres store: not initialized")
	}
	provider := strings.TrimSpace(r.Provider)
	state := strings.TrimSpace(r.State)
	if provider == "" || state == "" {
		return fmt.Errorf("postgres store: provider and state are required")
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (provider, state, code, error, created_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (provider, state) DO UPDATE
		SET code = EXCLUDED.code,
		    error = EXCLUDED.error,
		    created_at = NOW()
	`, s.fullTableName(defaultOAuthCallbackTable)), provider, state, strings.TrimSpace(r.Code), strings.TrimSpace(r.Error))
	return err
}

func (s *PostgresStore) GetOAuthCallback(ctx context.Context, provider, state string) (*OAuthCallbackRecord, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("postgres store: not initialized")
	}
	provider = strings.TrimSpace(provider)
	state = strings.TrimSpace(state)
	if provider == "" || state == "" {
		return nil, fmt.Errorf("postgres store: provider and state are required")
	}

	row := s.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT provider, state, code, error, created_at
		FROM %s
		WHERE provider = $1 AND state = $2
	`, s.fullTableName(defaultOAuthCallbackTable)), provider, state)

	var rec OAuthCallbackRecord
	if err := row.Scan(&rec.Provider, &rec.State, &rec.Code, &rec.Error, &rec.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}

func (s *PostgresStore) DeleteOAuthCallback(ctx context.Context, provider, state string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres store: not initialized")
	}
	provider = strings.TrimSpace(provider)
	state = strings.TrimSpace(state)
	if provider == "" || state == "" {
		return fmt.Errorf("postgres store: provider and state are required")
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		DELETE FROM %s
		WHERE provider = $1 AND state = $2
	`, s.fullTableName(defaultOAuthCallbackTable)), provider, state)
	return err
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
	if params.AuthID != "" {
		where = append(where, fmt.Sprintf("auth_id = $%d", argIdx))
		args = append(args, params.AuthID)
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
	AuthID  string
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
