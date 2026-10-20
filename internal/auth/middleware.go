package auth

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"

	appmiddleware "github.com/LeXwDeX/LLM-Proxy/internal/middleware"
)

type contextKey string

const (
	principalContextKey   contextKey = "auth_principal"
	revalidatorContextKey contextKey = "auth_revalidator"
)

// WithPrincipal stores the authenticated principal in the context.
func WithPrincipal(ctx context.Context, principal *Principal) context.Context {
	return context.WithValue(ctx, principalContextKey, principal)
}

// PrincipalFromContext returns the authenticated principal.
func PrincipalFromContext(ctx context.Context) (*Principal, bool) {
	if ctx == nil {
		return nil, false
	}
	principal, ok := ctx.Value(principalContextKey).(*Principal)
	return principal, ok && principal != nil
}

// Middleware returns an HTTP middleware enforcing Authorization: Bearer headers.
func Middleware(store *Store, logger *slog.Logger) func(http.Handler) http.Handler {
	trustProxyXFF := strings.EqualFold(strings.TrimSpace(os.Getenv("AUTH_TRUST_PROXY_XFF")), "true")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, err := authenticate(store, r)
			if err != nil {
				logger.Warn("authentication failed",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"remote_ip", r.RemoteAddr,
					"reason", err.Error(),
				)
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if principal.HasIPRestriction() {
				address, ok := sourceIP(r, trustProxyXFF)
				if !ok || !principal.CanAccessIP(address) {
					logger.Warn("client IP not allowed",
						"request_id", appmiddleware.RequestIDFromContext(r.Context()),
						"remote_ip", r.RemoteAddr,
						"client", principal.Name,
					)
					http.Error(w, "client IP is not allowed", http.StatusForbidden)
					return
				}
			}

			ctx := WithPrincipal(r.Context(), principal)
			ctx = context.WithValue(ctx, revalidatorContextKey, func(request *http.Request) (*Principal, int) {
				fresh, err := authenticate(store, request)
				if err != nil {
					return nil, http.StatusUnauthorized
				}
				if fresh.HasIPRestriction() {
					address, ok := sourceIP(request, trustProxyXFF)
					if !ok || !fresh.CanAccessIP(address) {
						return nil, http.StatusForbidden
					}
				}
				return fresh, 0
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// sourceIP uses the TCP peer by default. A deployment behind a trusted
// reverse proxy may opt into X-Forwarded-For only when that peer is private or
// loopback; the proxy must overwrite the header and be the only ingress.
func sourceIP(r *http.Request, trustProxyXFF bool) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	if trustProxyXFF && (peer.IsPrivate() || peer.IsLoopback()) {
		forwarded := r.Header.Get("X-Forwarded-For")
		if forwarded == "" {
			return netip.Addr{}, false
		}
		first, _, _ := strings.Cut(forwarded, ",")
		address, err := netip.ParseAddr(strings.TrimSpace(first))
		if err != nil {
			return netip.Addr{}, false
		}
		return address, true
	}
	return peer, true
}

func authenticate(store *Store, r *http.Request) (*Principal, error) {
	token := extractAccessKey(r)
	if token == "" {
		return nil, ErrMissingAuthorization
	}

	principal, ok := store.Authenticate(token)
	if !ok {
		return nil, ErrInvalidCredential
	}
	return principal, nil
}

func extractAccessKey(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			token := strings.TrimSpace(parts[1])
			if token != "" {
				return token
			}
		}
	}
	if apiKey := strings.TrimSpace(r.Header.Get("api-key")); apiKey != "" {
		return apiKey
	}
	if apiKey := strings.TrimSpace(r.Header.Get("x-api-key")); apiKey != "" {
		return apiKey
	}
	if apiKey := strings.TrimSpace(r.Header.Get("x-goog-api-key")); apiKey != "" {
		return apiKey
	}
	if r.URL != nil {
		if qKey := extractAPIKeyFromQuery(r.URL.RawQuery); qKey != "" {
			return qKey
		}
	}
	return ""
}

func extractAPIKeyFromQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	pairs := strings.Split(rawQuery, "&")
	for _, pair := range pairs {
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, val := strings.TrimSpace(kv[0]), kv[1]
		if !strings.EqualFold(key, "api-key") && !strings.EqualFold(key, "key") {
			continue
		}
		decoded, err := url.PathUnescape(strings.TrimSpace(val))
		if err != nil {
			continue
		}
		if decoded != "" {
			return decoded
		}
	}
	return ""
}

var (
	ErrMissingAuthorization = errors.New("authorization header missing or malformed")
	ErrInvalidCredential    = errors.New("invalid access key")
)

// RevalidateRequest refreshes credentials, ACL and IP policy for a new operation
// on a long-lived connection. Direct callers with a principal keep their contract.
func RevalidateRequest(r *http.Request) (*Principal, int) {
	if refresh, ok := r.Context().Value(revalidatorContextKey).(func(*http.Request) (*Principal, int)); ok {
		return refresh(r)
	}
	if principal, ok := PrincipalFromContext(r.Context()); ok {
		return principal, 0
	}
	return nil, http.StatusUnauthorized
}
