package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ListNodeRecords returns all node records with timestamps.
func (h *Handler) ListNodeRecords(c *gin.Context) {
	if h == nil || h.pgStore == nil {
		c.JSON(http.StatusOK, gin.H{"nodes": []NodeRecord{}})
		return
	}
	records, err := h.pgStore.ListNodeRecords(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if records == nil {
		records = []NodeRecord{}
	}
	c.JSON(http.StatusOK, gin.H{"nodes": records})
}

// CreateNode inserts or updates a node record in the database.
func (h *Handler) CreateNode(c *gin.Context) {
	if h == nil || h.pgStore == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "database store not configured"})
		return
	}
	var body struct {
		NodeIP       string `json:"node_ip"`
		RegisteredAt string `json:"registered_at"`
		LastSeenAt   string `json:"last_seen_at"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	nodeIP := strings.TrimSpace(body.NodeIP)
	if nodeIP == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node_ip is required"})
		return
	}
	record := NodeRecord{NodeIP: nodeIP}
	if strings.TrimSpace(body.RegisteredAt) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(body.RegisteredAt))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid registered_at"})
			return
		}
		record.RegisteredAt = t
	}
	if strings.TrimSpace(body.LastSeenAt) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(body.LastSeenAt))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid last_seen_at"})
			return
		}
		record.LastSeenAt = t
	}
	if err := h.pgStore.SaveNode(c.Request.Context(), record); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// RenameNode updates node_ip across node registry and related data tables.
func (h *Handler) RenameNode(c *gin.Context) {
	if h == nil || h.pgStore == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "database store not configured"})
		return
	}
	var body struct {
		OldNodeIP string `json:"old_node_ip"`
		NewNodeIP string `json:"new_node_ip"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	oldNodeIP := strings.TrimSpace(body.OldNodeIP)
	newNodeIP := strings.TrimSpace(body.NewNodeIP)
	if oldNodeIP == "" || newNodeIP == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "old_node_ip and new_node_ip are required"})
		return
	}
	if err := h.pgStore.RenameNode(c.Request.Context(), oldNodeIP, newNodeIP); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// DeleteNode removes a node record and its node-scoped auth/usage data.
func (h *Handler) DeleteNode(c *gin.Context) {
	if h == nil || h.pgStore == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "database store not configured"})
		return
	}
	nodeIP := strings.TrimSpace(c.Query("node_ip"))
	if nodeIP == "" {
		var body struct {
			NodeIP string `json:"node_ip"`
		}
		if err := c.ShouldBindJSON(&body); err == nil {
			nodeIP = strings.TrimSpace(body.NodeIP)
		}
	}
	if nodeIP == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node_ip is required"})
		return
	}
	if err := h.pgStore.DeleteNode(c.Request.Context(), nodeIP); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
