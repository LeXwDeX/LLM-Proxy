package admin

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/google/uuid"
)

// Preflight denials have their own audit transaction. They do not mutate usage.
func (h *Handler) auditUsageRecheckRejection(r *http.Request, reason string) {
	session, ok := SessionFromContext(r.Context())
	logger := h.logger
	if logger == nil {
		logger = slog.Default()
	}
	if !ok || session.Username == "" {
		logger.Error("usage recheck rejection audit unavailable: session identity missing", "reason", reason)
		return
	}
	if h.auditStore == nil {
		logger.Error("usage recheck rejection audit unavailable", "reason", reason)
		return
	}
	defer func() {
		if recover() != nil {
			logger.Error("usage recheck rejection audit write failed", "reason", reason)
		}
	}()
	if err := h.auditStore.Record(nosql.AuditEvent{Actor: session.Username, Action: "usage_recheck", Result: "rejected", Detail: reason}); err != nil {
		logger.Error("usage recheck rejection audit write failed", "reason", reason)
	}
}

func (h *Handler) auditReadonlyUsageRecheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if session, ok := SessionFromContext(r.Context()); ok && session.Role == RoleReadonly {
			h.auditUsageRecheckRejection(r, "readonly")
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) rejectInvalidUsageRecheck(w http.ResponseWriter, r *http.Request) {
	h.auditUsageRecheckRejection(r, "invalid_request")
	writeJSON(w, http.StatusBadRequest, errorResponse("invalid request"))
}

func (h *Handler) handleListUsageUnresolved(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	for key, values := range query {
		if (key != "offset" && key != "limit") || len(values) != 1 {
			writeJSON(w, 400, errorResponse("invalid query"))
			return
		}
	}
	offset, limit := 0, 100
	var err error
	if query.Has("offset") {
		offset, err = strconv.Atoi(query.Get("offset"))
		if err != nil || offset < 0 {
			writeJSON(w, 400, errorResponse("invalid offset"))
			return
		}
	}
	if query.Has("limit") {
		limit, err = strconv.Atoi(query.Get("limit"))
		if err != nil || limit < 1 || limit > 500 {
			writeJSON(w, 400, errorResponse("invalid limit"))
			return
		}
	}
	if h.usageStore == nil {
		writeJSON(w, 503, errorResponse("usage store unavailable"))
		return
	}
	entries, more, err := h.usageStore.ListUsageRechecks(offset, limit)
	if err != nil {
		writeJSON(w, 500, errorResponse("usage evidence unavailable"))
		return
	}
	writeJSON(w, 200, map[string]any{"entries": entries, "more": more})
}

func (h *Handler) handleUsageRecheck(w http.ResponseWriter, r *http.Request) {
	session, ok := SessionFromContext(r.Context())
	if !ok || session.Role != RoleAdmin || session.Username == "" {
		if ok && session.Role != RoleReadonly {
			h.auditUsageRecheckRejection(r, "admin_required")
		}
		writeJSON(w, http.StatusForbidden, errorResponse("admin role required"))
		return
	}
	if r.URL.RawQuery != "" {
		h.rejectInvalidUsageRecheck(w, r)
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		h.rejectInvalidUsageRecheck(w, r)
		return
	}
	fields := make(map[string]string)
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || (name != "evidence_id" && name != "version" && name != "operation_id") {
			h.rejectInvalidUsageRecheck(w, r)
			return
		}
		if _, exists := fields[name]; exists {
			h.rejectInvalidUsageRecheck(w, r)
			return
		}
		var value string
		if decoder.Decode(&value) != nil {
			h.rejectInvalidUsageRecheck(w, r)
			return
		}
		fields[name] = value
	}
	if _, err = decoder.Token(); err != nil {
		h.rejectInvalidUsageRecheck(w, r)
		return
	}
	if _, err = decoder.Token(); err != io.EOF {
		h.rejectInvalidUsageRecheck(w, r)
		return
	}
	for _, key := range []string{"evidence_id", "version"} {
		v := fields[key]
		decoded, err := hex.DecodeString(v)
		if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != v {
			h.rejectInvalidUsageRecheck(w, r)
			return
		}
	}
	id, err := uuid.Parse(fields["operation_id"])
	if err != nil || id.String() != fields["operation_id"] {
		h.rejectInvalidUsageRecheck(w, r)
		return
	}
	if h.usageStore == nil {
		writeJSON(w, 503, errorResponse("usage store unavailable"))
		return
	}
	result, err := h.usageStore.RecheckUsage(nosql.UsageRecheckRequest{EvidenceID: fields["evidence_id"], Version: fields["version"], OperationID: fields["operation_id"], Actor: session.Username})
	if errors.Is(err, nosql.ErrUsageRecheckConflict) {
		writeJSON(w, 409, errorResponse("usage recheck conflict"))
		return
	}
	if err != nil {
		writeJSON(w, 500, errorResponse("usage recheck unavailable"))
		return
	}
	writeJSON(w, 200, result)
}
