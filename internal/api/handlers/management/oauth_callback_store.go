package management

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (h *Handler) persistOAuthCallback(ctx context.Context, provider, state, code, errorMessage string) error {
	canonicalProvider, err := NormalizeOAuthProvider(provider)
	if err != nil {
		return err
	}
	if !IsOAuthSessionPending(state, canonicalProvider) {
		return errOAuthSessionNotPending
	}
	if h != nil && h.pgStore != nil {
		return h.pgStore.SaveOAuthCallback(ctx, OAuthCallbackPayload{
			Provider: canonicalProvider,
			Code:     strings.TrimSpace(code),
			State:    strings.TrimSpace(state),
			Error:    strings.TrimSpace(errorMessage),
		})
	}
	_, err = WriteOAuthCallbackFile(h.cfg.AuthDir, canonicalProvider, state, code, errorMessage)
	return err
}

// PersistOAuthCallback stores the short-lived OAuth callback payload using the
// configured persistence backend. PG mode uses the database; file mode falls back
// to the legacy auth-dir callback file.
func (h *Handler) PersistOAuthCallback(ctx context.Context, provider, state, code, errorMessage string) error {
	return h.persistOAuthCallback(ctx, provider, state, code, errorMessage)
}

func (h *Handler) waitForOAuthCallback(ctx context.Context, provider, state string, timeout time.Duration) (map[string]string, error) {
	canonicalProvider, err := NormalizeOAuthProvider(provider)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	for {
		if !IsOAuthSessionPending(state, canonicalProvider) {
			return nil, errOAuthSessionNotPending
		}
		if time.Now().After(deadline) {
			SetOAuthSessionError(state, "Timeout waiting for OAuth callback")
			return nil, fmt.Errorf("timeout waiting for OAuth callback")
		}

		if h != nil && h.pgStore != nil {
			payload, errGet := h.pgStore.GetOAuthCallback(ctx, canonicalProvider, state)
			if errGet != nil {
				return nil, errGet
			}
			if payload != nil {
				_ = h.pgStore.DeleteOAuthCallback(ctx, canonicalProvider, state)
				return map[string]string{
					"code":  strings.TrimSpace(payload.Code),
					"state": strings.TrimSpace(payload.State),
					"error": strings.TrimSpace(payload.Error),
				}, nil
			}
		} else {
			if result, ok, errRead := h.readOAuthCallbackFile(canonicalProvider, state); errRead != nil {
				return nil, errRead
			} else if ok {
				return result, nil
			}
		}

		time.Sleep(500 * time.Millisecond)
	}
}

func (h *Handler) readOAuthCallbackFile(provider, state string) (map[string]string, bool, error) {
	if h == nil || h.cfg == nil {
		return nil, false, fmt.Errorf("handler not initialized")
	}
	waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-%s-%s.oauth", provider, state))
	data, err := os.ReadFile(waitFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var payload oauthCallbackFilePayload
	if err = json.Unmarshal(data, &payload); err != nil {
		return nil, false, err
	}
	_ = os.Remove(waitFile)
	return map[string]string{
		"code":  strings.TrimSpace(payload.Code),
		"state": strings.TrimSpace(payload.State),
		"error": strings.TrimSpace(payload.Error),
	}, true, nil
}
