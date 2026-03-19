package main

import (
	"context"
	"time"

	configaccess "proxycore/api/v6/internal/access/config_access"
	"proxycore/api/v6/internal/config"
	"proxycore/api/v6/internal/store"
	log "github.com/sirupsen/logrus"
)

// loadAPIKeysFromDB fetches api_key records from the database and merges them into cfg.
// Records that are disabled are excluded.
func loadAPIKeysFromDB(cfg *config.Config, s *store.PostgresStore) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	records, err := s.ListAPIKeys(ctx)
	if err != nil {
		log.WithError(err).Warn("failed to load api_keys from database")
		return
	}
	if len(records) == 0 {
		return
	}
	keys := make([]string, 0, len(records))
	quotas := make(map[string]float64)
	for _, r := range records {
		if r.Disabled {
			continue
		}
		keys = append(keys, r.Key)
		if r.QuotaMillion > 0 {
			quotas[r.Key] = r.QuotaMillion
		}
	}
	// Merge with config-file keys (config keys remain as fallback).
	merged := append(append([]string(nil), cfg.SDKConfig.APIKeys...), keys...)
	cfg.SDKConfig.APIKeys = dedupStrings(merged)
	if cfg.SDKConfig.APIKeyQuotas == nil {
		cfg.SDKConfig.APIKeyQuotas = make(map[string]float64)
	}
	for k, v := range quotas {
		cfg.SDKConfig.APIKeyQuotas[k] = v
	}
	log.Infof("loaded %d api_keys from database (total active: %d)", len(keys), len(cfg.SDKConfig.APIKeys))
}

// pollAPIKeys periodically reloads api_keys from the database and hot-updates the access provider.
func pollAPIKeys(s *store.PostgresStore, cfg *config.Config) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		records, err := s.ListAPIKeys(ctx)
		cancel()
		if err != nil {
			log.WithError(err).Warn("api_keys poll: failed to load from database")
			continue
		}
		keys := make([]string, 0, len(records))
		quotas := make(map[string]float64)
		for _, r := range records {
			if r.Disabled {
				continue
			}
			keys = append(keys, r.Key)
			if r.QuotaMillion > 0 {
				quotas[r.Key] = r.QuotaMillion
			}
		}
		// Always include config-file keys as baseline.
		merged := append(append([]string(nil), cfg.SDKConfig.APIKeys...), keys...)
		merged = dedupStrings(merged)
		configaccess.UpdateKeys(merged)
	}
}

func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
