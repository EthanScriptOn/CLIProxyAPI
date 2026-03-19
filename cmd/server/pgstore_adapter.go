package main

import (
	"context"

	management "proxycore/api/v6/internal/api/handlers/management"
	"proxycore/api/v6/internal/store"
	"proxycore/api/v6/internal/usage"
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

