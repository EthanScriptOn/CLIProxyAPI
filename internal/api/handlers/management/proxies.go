package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (h *Handler) ListProxies(c *gin.Context) {
	if h == nil || h.pgStore == nil {
		c.JSON(http.StatusOK, gin.H{"proxies": []ProxyRecord{}})
		return
	}
	records, err := h.pgStore.ListProxies(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if records == nil {
		records = []ProxyRecord{}
	}
	c.JSON(http.StatusOK, gin.H{"proxies": records})
}

func (h *Handler) CreateProxy(c *gin.Context) {
	if h == nil || h.pgStore == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "database store not configured"})
		return
	}
	var body struct {
		Name        string `json:"name"`
		ProxyURL    string `json:"proxy_url"`
		Description string `json:"description"`
		Enabled     *bool  `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	name := strings.TrimSpace(body.Name)
	proxyURL := strings.TrimSpace(body.ProxyURL)
	if name == "" || proxyURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and proxy_url are required"})
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	if err := h.pgStore.SaveProxy(c.Request.Context(), ProxyRecord{
		Name:        name,
		ProxyURL:    proxyURL,
		Description: strings.TrimSpace(body.Description),
		Enabled:     enabled,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) UpdateProxy(c *gin.Context) {
	if h == nil || h.pgStore == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "database store not configured"})
		return
	}
	var body struct {
		OldName     string `json:"old_name"`
		Name        string `json:"name"`
		ProxyURL    string `json:"proxy_url"`
		Description string `json:"description"`
		Enabled     *bool  `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	oldName := strings.TrimSpace(body.OldName)
	name := strings.TrimSpace(body.Name)
	proxyURL := strings.TrimSpace(body.ProxyURL)
	if oldName == "" || name == "" || proxyURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "old_name, name and proxy_url are required"})
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	if err := h.pgStore.UpdateProxy(c.Request.Context(), oldName, ProxyRecord{
		Name:        name,
		ProxyURL:    proxyURL,
		Description: strings.TrimSpace(body.Description),
		Enabled:     enabled,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) DeleteProxy(c *gin.Context) {
	if h == nil || h.pgStore == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "database store not configured"})
		return
	}
	name := strings.TrimSpace(c.Param("name"))
	if name == "" {
		name = strings.TrimSpace(c.Query("name"))
	}
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if err := h.pgStore.DeleteProxy(c.Request.Context(), name); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
