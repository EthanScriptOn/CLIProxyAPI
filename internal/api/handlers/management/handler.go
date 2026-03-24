// Package management provides the management API handlers and middleware
// for configuring the server and managing auth files.
package management

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"proxycore/api/v6/internal/buildinfo"
	"proxycore/api/v6/internal/config"
	"proxycore/api/v6/internal/usage"
	sdkaccess "proxycore/api/v6/sdk/access"
	sdkAuth "proxycore/api/v6/sdk/auth"
	coreauth "proxycore/api/v6/sdk/cliproxy/auth"
)

type attemptInfo struct {
	count        int
	blockedUntil time.Time
	lastActivity time.Time // track last activity for cleanup
}

type authResult struct {
	OK         bool
	StatusCode int
	Message    string
}

// attemptCleanupInterval controls how often stale IP entries are purged
const attemptCleanupInterval = 1 * time.Hour

// attemptMaxIdleTime controls how long an IP can be idle before cleanup
const attemptMaxIdleTime = 2 * time.Hour

// Handler aggregates config reference, persistence path and helpers.
type Handler struct {
	cfg                 *config.Config
	configFilePath      string
	configPersister     ConfigPersister // non-nil in PG mode
	mu                  sync.Mutex
	attemptsMu          sync.Mutex
	failedAttempts      map[string]*attemptInfo // keyed by client IP
	authManager         *coreauth.Manager
	accessManager       *sdkaccess.Manager
	usageStats          *usage.RequestStatistics
	tokenStore          coreauth.Store
	localPassword       string
	allowRemoteOverride bool
	envSecret           string
	logDir              string
	postAuthHook        coreauth.PostAuthHook
	pgStore             DBAPIKeyStore // optional, non-nil when Postgres is configured
}

// DBAPIKeyStore abstracts postgres api_keys and usage_records operations.
type DBAPIKeyStore interface {
	ListAPIKeys(ctx context.Context) ([]APIKeyRecord, error)
	SaveAPIKey(ctx context.Context, r APIKeyRecord) error
	DeleteAPIKey(ctx context.Context, key string) error
	QueryUsageAggregate(ctx context.Context, params UsageAggregateParams) ([]UsageAggregateRow, error)
	ListNodes(ctx context.Context) ([]string, error)
	ListNodeRecords(ctx context.Context) ([]NodeRecord, error)
	SaveNode(ctx context.Context, r NodeRecord) error
	RenameNode(ctx context.Context, oldNodeIP, newNodeIP string) error
	DeleteNode(ctx context.Context, nodeIP string) error
	ListProxies(ctx context.Context) ([]ProxyRecord, error)
	SaveProxy(ctx context.Context, r ProxyRecord) error
	UpdateProxy(ctx context.Context, oldName string, r ProxyRecord) error
	DeleteProxy(ctx context.Context, name string) error
	GetManagementPasswordHash(ctx context.Context) (string, error)
	SetManagementPasswordHash(ctx context.Context, hash string) error
	ListAuthByNode(ctx context.Context, nodeIP string) ([]*coreauth.Auth, error)
	ListAllAuth(ctx context.Context) ([]*coreauth.Auth, error)
	DeleteAuth(ctx context.Context, id string) error
	DeleteAllAuth(ctx context.Context) error
}

// ConfigPersister abstracts config persistence for PG mode (no local file).
type ConfigPersister interface {
	GetConfigContent(ctx context.Context) (string, error)
	SaveConfigContent(ctx context.Context, content string) error
}

// NewHandler creates a new management handler instance.
func NewHandler(cfg *config.Config, configFilePath string, manager *coreauth.Manager) *Handler {
	envSecret, _ := os.LookupEnv("MANAGEMENT_PASSWORD")
	envSecret = strings.TrimSpace(envSecret)

	h := &Handler{
		cfg:                 cfg,
		configFilePath:      configFilePath,
		failedAttempts:      make(map[string]*attemptInfo),
		authManager:         manager,
		usageStats:          usage.GetRequestStatistics(),
		tokenStore:          sdkAuth.GetTokenStore(),
		allowRemoteOverride: envSecret != "",
		envSecret:           envSecret,
	}
	h.startAttemptCleanup()
	return h
}

// startAttemptCleanup launches a background goroutine that periodically
// removes stale IP entries from failedAttempts to prevent memory leaks.
func (h *Handler) startAttemptCleanup() {
	go func() {
		ticker := time.NewTicker(attemptCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			h.purgeStaleAttempts()
		}
	}()
}

// purgeStaleAttempts removes IP entries that have been idle beyond attemptMaxIdleTime
// and whose ban (if any) has expired.
func (h *Handler) purgeStaleAttempts() {
	now := time.Now()
	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()
	for ip, ai := range h.failedAttempts {
		// Skip if still banned
		if !ai.blockedUntil.IsZero() && now.Before(ai.blockedUntil) {
			continue
		}
		// Remove if idle too long
		if now.Sub(ai.lastActivity) > attemptMaxIdleTime {
			delete(h.failedAttempts, ip)
		}
	}
}

// NewHandler creates a new management handler instance.
func NewHandlerWithoutConfigFilePath(cfg *config.Config, manager *coreauth.Manager) *Handler {
	return NewHandler(cfg, "", manager)
}

// SetConfig updates the in-memory config reference when the server hot-reloads.
func (h *Handler) SetConfig(cfg *config.Config) { h.cfg = cfg }

// HasDBStore reports whether management authentication/persistence is backed by Postgres.
func (h *Handler) HasDBStore() bool { return h != nil && h.pgStore != nil }

// SetAuthManager updates the auth manager reference used by management endpoints.
func (h *Handler) SetAuthManager(manager *coreauth.Manager) { h.authManager = manager }

// SetUsageStatistics allows replacing the usage statistics reference.
func (h *Handler) SetUsageStatistics(stats *usage.RequestStatistics) { h.usageStats = stats }

// SetAccessManager sets the access manager used to propagate api-key changes at runtime.
func (h *Handler) SetAccessManager(m *sdkaccess.Manager) { h.accessManager = m }

// SetLocalPassword configures the runtime-local password accepted for localhost requests.
func (h *Handler) SetLocalPassword(password string) { h.localPassword = password }

// SetLogDirectory updates the directory where main.log should be looked up.
func (h *Handler) SetLogDirectory(dir string) {
	if dir == "" {
		return
	}
	if !filepath.IsAbs(dir) {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
	}
	h.logDir = dir
}

// SetPostAuthHook registers a hook to be called after auth record creation but before persistence.
func (h *Handler) SetPostAuthHook(hook coreauth.PostAuthHook) {
	h.postAuthHook = hook
}

// SetConfigPersister registers a ConfigPersister for PG mode; when set, persist() uses it instead of files.
func (h *Handler) SetConfigPersister(p ConfigPersister) {
	h.configPersister = p
}

func (h *Handler) managementSecrets(ctx context.Context) (allowRemote bool, secretHash string, envSecret string, dbHash string) {
	cfg := h.cfg
	if cfg != nil {
		allowRemote = cfg.RemoteManagement.AllowRemote
		secretHash = cfg.RemoteManagement.SecretKey
	}
	if h.allowRemoteOverride || h.pgStore != nil {
		allowRemote = true
	}
	envSecret = h.envSecret
	if h.pgStore != nil {
		dbHash, _ = h.pgStore.GetManagementPasswordHash(ctx)
	}
	return allowRemote, secretHash, envSecret, dbHash
}

func (h *Handler) clearFailure(clientIP string) {
	if clientIP == "" {
		return
	}
	h.attemptsMu.Lock()
	if ai := h.failedAttempts[clientIP]; ai != nil {
		ai.count = 0
		ai.blockedUntil = time.Time{}
	}
	h.attemptsMu.Unlock()
}

func (h *Handler) recordFailure(clientIP string, maxFailures int, banDuration time.Duration) {
	if clientIP == "" {
		return
	}
	h.attemptsMu.Lock()
	aip := h.failedAttempts[clientIP]
	if aip == nil {
		aip = &attemptInfo{}
		h.failedAttempts[clientIP] = aip
	}
	aip.count++
	aip.lastActivity = time.Now()
	if aip.count >= maxFailures {
		aip.blockedUntil = time.Now().Add(banDuration)
		aip.count = 0
	}
	h.attemptsMu.Unlock()
}

func (h *Handler) checkBan(clientIP string) authResult {
	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()
	ai := h.failedAttempts[clientIP]
	if ai == nil {
		return authResult{}
	}
	if ai.blockedUntil.IsZero() {
		return authResult{}
	}
	if time.Now().Before(ai.blockedUntil) {
		remaining := time.Until(ai.blockedUntil).Round(time.Second)
		return authResult{
			StatusCode: http.StatusForbidden,
			Message:    fmt.Sprintf("IP banned due to too many failed attempts. Try again in %s", remaining),
		}
	}
	ai.blockedUntil = time.Time{}
	ai.count = 0
	return authResult{}
}

func (h *Handler) validateManagementKey(ctx context.Context, clientIP, provided string, localClient bool) authResult {
	const (
		maxFailures = 5
		banDuration = 30 * time.Minute
	)

	allowRemote, secretHash, envSecret, dbHash := h.managementSecrets(ctx)

	if !localClient {
		if banned := h.checkBan(clientIP); banned.StatusCode != 0 {
			return banned
		}
		if !allowRemote {
			return authResult{StatusCode: http.StatusForbidden, Message: "remote management disabled"}
		}
	}

	if dbHash == "" && secretHash == "" && envSecret == "" {
		return authResult{StatusCode: http.StatusForbidden, Message: "remote management key not set"}
	}

	if strings.TrimSpace(provided) == "" {
		if !localClient {
			h.recordFailure(clientIP, maxFailures, banDuration)
		}
		return authResult{StatusCode: http.StatusUnauthorized, Message: "missing management key"}
	}

	if localClient {
		if lp := h.localPassword; lp != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(lp)) == 1 {
			return authResult{OK: true}
		}
	}

	if envSecret != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(envSecret)) == 1 {
		if !localClient {
			h.clearFailure(clientIP)
		}
		return authResult{OK: true}
	}

	if dbHash != "" {
		if bcrypt.CompareHashAndPassword([]byte(dbHash), []byte(provided)) == nil {
			if !localClient {
				h.clearFailure(clientIP)
			}
			return authResult{OK: true}
		}
		if !localClient {
			h.recordFailure(clientIP, maxFailures, banDuration)
		}
		return authResult{StatusCode: http.StatusUnauthorized, Message: "invalid management key"}
	}

	if secretHash == "" || bcrypt.CompareHashAndPassword([]byte(secretHash), []byte(provided)) != nil {
		if !localClient {
			h.recordFailure(clientIP, maxFailures, banDuration)
		}
		return authResult{StatusCode: http.StatusUnauthorized, Message: "invalid management key"}
	}

	if !localClient {
		h.clearFailure(clientIP)
	}
	return authResult{OK: true}
}

func (h *Handler) validateDatabaseManagementKey(ctx context.Context, clientIP, provided string) authResult {
	const (
		maxFailures = 5
		banDuration = 30 * time.Minute
	)

	if h == nil || h.pgStore == nil {
		return authResult{StatusCode: http.StatusServiceUnavailable, Message: "database-backed login unavailable"}
	}

	if banned := h.checkBan(clientIP); banned.StatusCode != 0 {
		return banned
	}

	if strings.TrimSpace(provided) == "" {
		h.recordFailure(clientIP, maxFailures, banDuration)
		return authResult{StatusCode: http.StatusUnauthorized, Message: "missing management key"}
	}

	dbHash, err := h.pgStore.GetManagementPasswordHash(ctx)
	if err != nil {
		return authResult{StatusCode: http.StatusInternalServerError, Message: "failed to load management password from database"}
	}
	if strings.TrimSpace(dbHash) == "" {
		return authResult{StatusCode: http.StatusForbidden, Message: "database management key not set"}
	}

	if bcrypt.CompareHashAndPassword([]byte(dbHash), []byte(provided)) != nil {
		h.recordFailure(clientIP, maxFailures, banDuration)
		return authResult{StatusCode: http.StatusUnauthorized, Message: "invalid management key"}
	}

	h.clearFailure(clientIP)
	return authResult{OK: true}
}

// Middleware enforces access control for management endpoints.
// All requests (local and remote) require a valid management key.
// Additionally, remote access requires allow-remote-management=true.
func (h *Handler) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-CPA-VERSION", buildinfo.Version)
		c.Header("X-CPA-COMMIT", buildinfo.Commit)
		c.Header("X-CPA-BUILD-DATE", buildinfo.BuildDate)

		clientIP := c.ClientIP()
		localClient := clientIP == "127.0.0.1" || clientIP == "::1"

		// Accept either Authorization: Bearer <key> or X-Management-Key
		var provided string
		if ah := c.GetHeader("Authorization"); ah != "" {
			parts := strings.SplitN(ah, " ", 2)
			if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" {
				provided = parts[1]
			} else {
				provided = ah
			}
		}
		if provided == "" {
			provided = c.GetHeader("X-Management-Key")
		}
		result := h.validateManagementKey(c.Request.Context(), clientIP, provided, localClient)
		if !result.OK {
			c.AbortWithStatusJSON(result.StatusCode, gin.H{"error": result.Message})
			return
		}
		c.Next()
	}
}

// persist saves the current in-memory config to storage (DB or file).
func (h *Handler) persist(c *gin.Context) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.configPersister != nil {
		// PG mode: fetch original YAML from DB, merge current config, save back.
		ctx := c.Request.Context()
		origContent, err := h.configPersister.GetConfigContent(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to load config from DB: %v", err)})
			return false
		}
		merged, err := config.MergeConfigToYAML([]byte(origContent), h.cfg)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to merge config: %v", err)})
			return false
		}
		if err = h.configPersister.SaveConfigContent(ctx, string(merged)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config to DB: %v", err)})
			return false
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return true
	}

	// File mode: preserve comments when writing.
	if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", err)})
		return false
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	return true
}

// loadCfgFromDB fetches and parses config from DB when in PG mode.
// Returns nil, nil if configPersister is not set (file mode).
func (h *Handler) loadCfgFromDB(ctx context.Context) (*config.Config, error) {
	if h.configPersister == nil {
		return nil, nil
	}
	content, err := h.configPersister.GetConfigContent(ctx)
	if err != nil {
		return nil, err
	}
	return config.ParseConfigContent(content, true)
}

// Helper methods for simple types
func (h *Handler) updateBoolField(c *gin.Context, set func(bool)) {
	var body struct {
		Value *bool `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	set(*body.Value)
	h.persist(c)
}

func (h *Handler) updateIntField(c *gin.Context, set func(int)) {
	var body struct {
		Value *int `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	set(*body.Value)
	h.persist(c)
}

func (h *Handler) updateStringField(c *gin.Context, set func(string)) {
	var body struct {
		Value *string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	set(*body.Value)
	h.persist(c)
}
