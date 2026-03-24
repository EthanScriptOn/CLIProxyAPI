package main

import (
	"context"

	management "proxycore/api/v6/internal/api/handlers/management"
	"proxycore/api/v6/internal/store"
	"proxycore/api/v6/internal/usage"
	coreauth "proxycore/api/v6/sdk/cliproxy/auth"
)

// pgStoreAdapter bridges store.PostgresStore to the management.DBAPIKeyStore interface.
type pgStoreAdapter struct {
	s *store.PostgresStore
}

func (a *pgStoreAdapter) ListAPIKeys(ctx context.Context) ([]management.APIKeyRecord, error) {
	raw, err := a.s.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]management.APIKeyRecord, len(raw))
	for i, r := range raw {
		out[i] = management.APIKeyRecord{
			Key:          r.Key,
			Label:        r.Label,
			QuotaMillion: r.QuotaMillion,
			Disabled:     r.Disabled,
			CreatedAt:    r.CreatedAt,
			UpdatedAt:    r.UpdatedAt,
		}
	}
	return out, nil
}

func (a *pgStoreAdapter) SaveAPIKey(ctx context.Context, r management.APIKeyRecord) error {
	return a.s.SaveAPIKey(ctx, store.APIKeyRecord{
		Key:          r.Key,
		Label:        r.Label,
		QuotaMillion: r.QuotaMillion,
		Disabled:     r.Disabled,
	})
}

func (a *pgStoreAdapter) DeleteAPIKey(ctx context.Context, key string) error {
	return a.s.DeleteAPIKey(ctx, key)
}

func (a *pgStoreAdapter) QueryUsageAggregate(ctx context.Context, params management.UsageAggregateParams) ([]management.UsageAggregateRow, error) {
	raw, err := a.s.QueryUsageAggregate(ctx, store.UsageAggregateParams{
		APIKey:  params.APIKey,
		NodeIP:  params.NodeIP,
		AuthID:  params.AuthID,
		From:    params.From,
		To:      params.To,
		GroupBy: params.GroupBy,
	})
	if err != nil {
		return nil, err
	}
	out := make([]management.UsageAggregateRow, len(raw))
	for i, r := range raw {
		out[i] = management.UsageAggregateRow{
			GroupKey:     r.GroupKey,
			Requests:     r.Requests,
			InputTokens:  r.InputTokens,
			OutputTokens: r.OutputTokens,
			TotalTokens:  r.TotalTokens,
			FailedCount:  r.FailedCount,
		}
	}
	return out, nil
}

// usageStoreAdapter bridges store.PostgresStore to usage.UsageStoreWriter.
type usageStoreAdapter struct {
	s      *store.PostgresStore
	nodeIP string
}

func (a *pgStoreAdapter) ListNodes(ctx context.Context) ([]string, error) {
	return a.s.ListNodes(ctx)
}

func (a *pgStoreAdapter) ListNodeRecords(ctx context.Context) ([]management.NodeRecord, error) {
	raw, err := a.s.ListNodeRecords(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]management.NodeRecord, len(raw))
	for i, r := range raw {
		out[i] = management.NodeRecord{
			NodeIP:       r.NodeIP,
			RegisteredAt: r.RegisteredAt,
			LastSeenAt:   r.LastSeenAt,
		}
	}
	return out, nil
}

func (a *pgStoreAdapter) SaveNode(ctx context.Context, r management.NodeRecord) error {
	return a.s.SaveNode(ctx, store.NodeRecord{
		NodeIP:       r.NodeIP,
		RegisteredAt: r.RegisteredAt,
		LastSeenAt:   r.LastSeenAt,
	})
}

func (a *pgStoreAdapter) RenameNode(ctx context.Context, oldNodeIP, newNodeIP string) error {
	return a.s.RenameNode(ctx, oldNodeIP, newNodeIP)
}

func (a *pgStoreAdapter) DeleteNode(ctx context.Context, nodeIP string) error {
	return a.s.DeleteNode(ctx, nodeIP)
}

func (a *pgStoreAdapter) ListProxies(ctx context.Context) ([]management.ProxyRecord, error) {
	raw, err := a.s.ListProxies(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]management.ProxyRecord, len(raw))
	for i, r := range raw {
		out[i] = management.ProxyRecord{
			Name:        r.Name,
			ProxyURL:    r.ProxyURL,
			Description: r.Description,
			Enabled:     r.Enabled,
			CreatedAt:   r.CreatedAt,
			UpdatedAt:   r.UpdatedAt,
		}
	}
	return out, nil
}

func (a *pgStoreAdapter) SaveProxy(ctx context.Context, r management.ProxyRecord) error {
	return a.s.SaveProxy(ctx, store.ProxyRecord{
		Name:        r.Name,
		ProxyURL:    r.ProxyURL,
		Description: r.Description,
		Enabled:     r.Enabled,
	})
}

func (a *pgStoreAdapter) UpdateProxy(ctx context.Context, oldName string, r management.ProxyRecord) error {
	return a.s.UpdateProxy(ctx, oldName, store.ProxyRecord{
		Name:        r.Name,
		ProxyURL:    r.ProxyURL,
		Description: r.Description,
		Enabled:     r.Enabled,
	})
}

func (a *pgStoreAdapter) DeleteProxy(ctx context.Context, name string) error {
	return a.s.DeleteProxy(ctx, name)
}

func (a *pgStoreAdapter) SaveOAuthCallback(ctx context.Context, payload management.OAuthCallbackPayload) error {
	return a.s.SaveOAuthCallback(ctx, store.OAuthCallbackRecord{
		Provider: payload.Provider,
		State:    payload.State,
		Code:     payload.Code,
		Error:    payload.Error,
	})
}

func (a *pgStoreAdapter) GetOAuthCallback(ctx context.Context, provider, state string) (*management.OAuthCallbackPayload, error) {
	rec, err := a.s.GetOAuthCallback(ctx, provider, state)
	if err != nil || rec == nil {
		return nil, err
	}
	return &management.OAuthCallbackPayload{
		Provider: rec.Provider,
		State:    rec.State,
		Code:     rec.Code,
		Error:    rec.Error,
	}, nil
}

func (a *pgStoreAdapter) DeleteOAuthCallback(ctx context.Context, provider, state string) error {
	return a.s.DeleteOAuthCallback(ctx, provider, state)
}

func (a *pgStoreAdapter) ListAuthByNode(ctx context.Context, nodeIP string) ([]*coreauth.Auth, error) {
	return a.s.ListAuthByNode(ctx, nodeIP)
}

func (a *pgStoreAdapter) ListAllAuth(ctx context.Context) ([]*coreauth.Auth, error) {
	return a.s.ListAllAuth(ctx)
}

func (a *pgStoreAdapter) DeleteAuth(ctx context.Context, id string) error {
	return a.s.DeleteAuthByID(ctx, id)
}

func (a *pgStoreAdapter) DeleteAllAuth(ctx context.Context) error {
	return a.s.DeleteAllAuth(ctx)
}

func (a *pgStoreAdapter) GetConfigContent(ctx context.Context) (string, error) {
	return a.s.GetConfigContent(ctx)
}

func (a *pgStoreAdapter) SaveConfigContent(ctx context.Context, content string) error {
	return a.s.SaveConfigContent(ctx, content)
}

// usageStoreAdapter bridges store.PostgresStore to usage.UsageStoreWriter.
func (a *usageStoreAdapter) InsertUsageRecord(ctx context.Context, r usage.UsageDBRecord) {
	nodeIP := r.NodeIP
	if nodeIP == "" {
		nodeIP = a.nodeIP
	}
	a.s.InsertUsageRecord(ctx, store.UsageRecord{
		APIKey:          r.APIKey,
		NodeIP:          nodeIP,
		Provider:        r.Provider,
		Model:           r.Model,
		AuthID:          r.AuthID,
		Source:          r.Source,
		InputTokens:     r.InputTokens,
		OutputTokens:    r.OutputTokens,
		ReasoningTokens: r.ReasoningTokens,
		CachedTokens:    r.CachedTokens,
		TotalTokens:     r.TotalTokens,
		Failed:          r.Failed,
		RequestedAt:     r.RequestedAt,
	})
}

func (a *pgStoreAdapter) GetManagementPasswordHash(ctx context.Context) (string, error) {
	return a.s.GetManagementPasswordHash(ctx)
}

func (a *pgStoreAdapter) SetManagementPasswordHash(ctx context.Context, hash string) error {
	return a.s.SetManagementPasswordHash(ctx, hash)
}
