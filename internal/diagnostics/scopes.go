package diagnostics

import (
	"errors"
	"net/http"
	"time"
)

const DiagnosticsVersion = 3

func MaximumScopes() []string { return []string{"read", "probe", "inspect", "maintain"} }

func hasScope(scopes []string, wanted string) bool {
	for _, scope := range scopes {
		if scope == wanted {
			return true
		}
	}
	return false
}

func normalizeScopes(scopes []string) ([]string, error) {
	seen := map[string]bool{}
	for _, scope := range scopes {
		if seen[scope] || !hasScope(MaximumScopes(), scope) {
			return nil, errors.New("无效或重复的调试权限")
		}
		seen[scope] = true
	}
	if !seen["read"] || (seen["maintain"] && !seen["inspect"]) {
		return nil, errors.New("必须包含 read，维护权限还需包含 inspect")
	}
	result := []string{}
	for _, scope := range MaximumScopes() {
		if seen[scope] {
			result = append(result, scope)
		}
	}
	return result, nil
}

func requestScopes(r *http.Request) []string {
	scopes, _ := r.Context().Value(scopeKey{}).([]string)
	return scopes
}

func requireScopes(w http.ResponseWriter, r *http.Request, required ...string) bool {
	for _, scope := range required {
		if !hasScope(requestScopes(r), scope) {
			failure(w, http.StatusForbidden, scope+"_scope_required")
			return false
		}
	}
	return true
}

func scopeTimeout(scopes []string) time.Duration {
	if hasScope(scopes, "inspect") {
		return 60 * time.Second
	}
	return 20 * time.Second
}

func scopeRate(scopes []string) int {
	if hasScope(scopes, "inspect") {
		return 120
	}
	return callsPerMinute
}

type ownerKey struct{}
type tokenIDKey struct{}
