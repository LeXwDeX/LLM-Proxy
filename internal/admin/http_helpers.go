package admin

import (
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// pathParam returns the URL-decoded path parameter. Go net/http does not
// decode %2F in path segments, so model names like "MiniMax/MiniMax-M3"
// arrive as "MiniMax%2FMiniMax-M3".
func pathParam(r *http.Request, name string) string {
	value, err := decodedPathParam(r, name)
	if err != nil {
		return chi.URLParam(r, name)
	}
	return value
}

// decodedPathParam decodes a path parameter exactly once. chi routes on
// RawPath, and so returns an escaped segment, only when the request needed
// one (for example %2F); otherwise the segment is already decoded and a
// second decode would turn a literal "a%41" into "aA".
func decodedPathParam(r *http.Request, name string) (string, error) {
	raw := chi.URLParam(r, name)
	if r.URL.RawPath == "" {
		return raw, nil
	}
	return url.PathUnescape(raw)
}

// pathParamLower is pathParam plus lowercase + trim normalization.
func pathParamLower(r *http.Request, name string) string {
	return strings.ToLower(strings.TrimSpace(pathParam(r, name)))
}

func (h *Handler) writeInternalError(w http.ResponseWriter, message string, err error) {
	h.logger.Error(message, "error", err)
	writeJSON(w, http.StatusInternalServerError, errorResponse("internal server error"))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		// Last resort logging; cannot change response as headers already sent.
		// Use standard logger to avoid recursive dependency.
		slog.Default().Error("failed to encode admin response", "error", err)
	}
}

func errorResponse(message string) map[string]string {
	return map[string]string{"error": message}
}
