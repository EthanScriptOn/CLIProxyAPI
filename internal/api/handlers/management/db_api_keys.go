package management

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	configaccess "proxycore/api/v6/internal/access/config_access"
	sdkaccess "proxycore/api/v6/sdk/access"
)

const dbKeySyncInterval = 10 * time.Second

// APIKeyRecord mirrors store.APIKeyRecord but lives in this package to avoid import cycles.
type APIKeyRecord struct {
	Key          string    `json:"key"`
	Label        string    `json:"label"`
	QuotaMillion float64   `json:"quota_million"`
	Disabled     bool      `json:"disabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// UsageAggregateParams mirrors store.UsageAggregateParams.
type UsageAggregateParams struct {
	APIKey  string
	NodeIP  string
	AuthID  string
	From    time.Time
	To      time.Time
	GroupBy string
}

// UsageAggregateRow mirrors store.UsageAggregateRow.
type UsageAggregateRow struct {
	GroupKey     string `json:"group_key"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	FailedCount  int64  `json:"failed_count"`
}

// SetPGStore injects the database store into the management handler.
func (h *Handler) SetPGStore(s DBAPIKeyStore) {
	if h == nil {
		return
	}
	h.pgStore = s
	if s != nil {
		go h.startDBKeySync()
	}
}

// startDBKeySync periodically reloads DB keys into the in-memory access provider.
func (h *Handler) startDBKeySync() {
	ticker := time.NewTicker(dbKeySyncInterval)
	defer ticker.Stop()
	for range ticker.C {
		if h.pgStore == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		h.syncDBKeysToMemory(ctx)
		cancel()
	}
}

// syncDBKeysToMemory reloads all non-disabled keys from the database into the in-memory access provider.
func (h *Handler) syncDBKeysToMemory(ctx context.Context) {
	if h.pgStore == nil {
		return
	}
	records, err := h.pgStore.ListAPIKeys(ctx)
	if err != nil {
		return
	}
	keys := make([]string, 0, len(records))
	for _, r := range records {
		if !r.Disabled {
			keys = append(keys, r.Key)
		}
	}
	configaccess.UpdateKeys(keys)
	// Re-snapshot providers in case UpdateKeys created a new provider instance.
	if h.accessManager != nil {
		h.accessManager.SetProviders(sdkaccess.RegisteredProviders())
	}
}

// ListDBAPIKeys returns all api_key records from the database.
// Falls back to the config.yaml list when no store is set.
func (h *Handler) ListDBAPIKeys(c *gin.Context) {
	if h == nil || h.pgStore == nil {
		h.GetAPIKeys(c)
		return
	}
	records, err := h.pgStore.ListAPIKeys(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if records == nil {
		records = []APIKeyRecord{}
	}
	c.JSON(http.StatusOK, gin.H{"api-keys": records})
}

// CreateDBAPIKey creates a new api_key record in the database.
func (h *Handler) CreateDBAPIKey(c *gin.Context) {
	if h == nil || h.pgStore == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "database store not configured"})
		return
	}
	var body struct {
		Key          string  `json:"key"`
		Label        string  `json:"label"`
		QuotaMillion float64 `json:"quota_million"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Key) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key is required"})
		return
	}
	r := APIKeyRecord{
		Key:          strings.TrimSpace(body.Key),
		Label:        body.Label,
		QuotaMillion: body.QuotaMillion,
	}
	if err := h.pgStore.SaveAPIKey(c.Request.Context(), r); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.syncDBKeysToMemory(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"status": "ok", "key": r.Key})
}

// DeleteDBAPIKey removes an api_key record from the database.
func (h *Handler) DeleteDBAPIKey(c *gin.Context) {
	if h == nil || h.pgStore == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "database store not configured"})
		return
	}
	key := strings.TrimSpace(c.Param("key"))
	if key == "" {
		key = strings.TrimSpace(c.Query("key"))
	}
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key is required"})
		return
	}
	if err := h.pgStore.DeleteAPIKey(c.Request.Context(), key); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.syncDBKeysToMemory(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// GetUsageAggregate queries aggregated usage from the database.
func (h *Handler) GetUsageAggregate(c *gin.Context) {
	if h == nil || h.pgStore == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "database store not configured"})
		return
	}
	params := UsageAggregateParams{
		APIKey:  strings.TrimSpace(c.Query("api_key")),
		NodeIP:  strings.TrimSpace(c.Query("node_ip")),
		AuthID:  strings.TrimSpace(c.Query("auth_id")),
		GroupBy: strings.TrimSpace(c.Query("group_by")),
	}
	if fromStr := c.Query("from"); fromStr != "" {
		if t, err := time.Parse(time.RFC3339, fromStr); err == nil {
			params.From = t
		}
	}
	if toStr := c.Query("to"); toStr != "" {
		if t, err := time.Parse(time.RFC3339, toStr); err == nil {
			params.To = t
		}
	}
	rows, err := h.pgStore.QueryUsageAggregate(context.Background(), params)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = []UsageAggregateRow{}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows})
}
