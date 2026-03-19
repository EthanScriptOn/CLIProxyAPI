package configaccess

import (
	"context"
	"net/http"
	"strings"
	"sync"

	sdkaccess "proxycore/api/v6/sdk/access"
	sdkconfig "proxycore/api/v6/sdk/config"
)

var (
	globalMu       sync.Mutex
	globalProvider *provider
)

// Register ensures the config-access provider is available to the access manager.
func Register(cfg *sdkconfig.SDKConfig) {
	if cfg == nil {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		globalMu.Lock()
		globalProvider = nil
		globalMu.Unlock()
		return
	}

	keys := normalizeKeys(cfg.APIKeys)
	if len(keys) == 0 {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		globalMu.Lock()
		globalProvider = nil
		globalMu.Unlock()
		return
	}

	p := newProvider(sdkaccess.DefaultAccessProviderName, keys)
	sdkaccess.RegisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey, p)
	globalMu.Lock()
	globalProvider = p
	globalMu.Unlock()
}

// UpdateKeys hot-swaps the key set of the currently registered provider.
// If no provider is registered yet, one is created and registered.
func UpdateKeys(keys []string) {
	normalized := normalizeKeys(keys)

	globalMu.Lock()
	p := globalProvider
	if p == nil && len(normalized) > 0 {
		p = newProvider(sdkaccess.DefaultAccessProviderName, nil)
		sdkaccess.RegisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey, p)
		globalProvider = p
	}
	globalMu.Unlock()

	if p != nil {
		p.UpdateKeys(normalized)
	}
	if len(normalized) == 0 {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
	}
}

type provider struct {
	name string
	mu   sync.RWMutex
	keys map[string]struct{}
}

func newProvider(name string, keys []string) *provider {
	providerName := strings.TrimSpace(name)
	if providerName == "" {
		providerName = sdkaccess.DefaultAccessProviderName
	}
	keySet := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		keySet[key] = struct{}{}
	}
	return &provider{name: providerName, keys: keySet}
}

// UpdateKeys replaces the current key set atomically.
func (p *provider) UpdateKeys(keys []string) {
	if p == nil {
		return
	}
	normalized := normalizeKeys(keys)
	keySet := make(map[string]struct{}, len(normalized))
	for _, k := range normalized {
		keySet[k] = struct{}{}
	}
	p.mu.Lock()
	p.keys = keySet
	p.mu.Unlock()
}

func (p *provider) Identifier() string {
	if p == nil || p.name == "" {
		return sdkaccess.DefaultAccessProviderName
	}
	return p.name
}

func (p *provider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p == nil {
		return nil, sdkaccess.NewNotHandledError()
	}
	p.mu.RLock()
	keyCount := len(p.keys)
	p.mu.RUnlock()
	if keyCount == 0 {
		return nil, sdkaccess.NewNotHandledError()
	}
	authHeader := r.Header.Get("Authorization")
	authHeaderGoogle := r.Header.Get("X-Goog-Api-Key")
	authHeaderAnthropic := r.Header.Get("X-Api-Key")
	queryKey := ""
	queryAuthToken := ""
	if r.URL != nil {
		queryKey = r.URL.Query().Get("key")
		queryAuthToken = r.URL.Query().Get("auth_token")
	}
	if authHeader == "" && authHeaderGoogle == "" && authHeaderAnthropic == "" && queryKey == "" && queryAuthToken == "" {
		return nil, sdkaccess.NewNoCredentialsError()
	}

	apiKey := extractBearerToken(authHeader)

	candidates := []struct {
		value  string
		source string
	}{
		{apiKey, "authorization"},
		{authHeaderGoogle, "x-goog-api-key"},
		{authHeaderAnthropic, "x-api-key"},
		{queryKey, "query-key"},
		{queryAuthToken, "query-auth-token"},
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, candidate := range candidates {
		if candidate.value == "" {
			continue
		}
		if _, ok := p.keys[candidate.value]; ok {
			return &sdkaccess.Result{
				Provider:  p.Identifier(),
				Principal: candidate.value,
				Metadata: map[string]string{
					"source": candidate.source,
				},
			}, nil
		}
	}

	return nil, sdkaccess.NewInvalidCredentialError()
}

func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return header
	}
	if strings.ToLower(parts[0]) != "bearer" {
		return header
	}
	return strings.TrimSpace(parts[1])
}

func normalizeKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		if _, exists := seen[trimmedKey]; exists {
			continue
		}
		seen[trimmedKey] = struct{}{}
		normalized = append(normalized, trimmedKey)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}
