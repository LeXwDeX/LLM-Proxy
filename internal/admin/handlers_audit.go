package admin

import (
	"errors"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (h *Handler) handleListAudit(w http.ResponseWriter, r *http.Request) {
	events, err := h.listAuditEventsFromRequest(r)
	if err != nil {
		h.writeInternalError(w, "failed to list audit events", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events": events,
		"count":  len(events),
	})
}

func (h *Handler) listAuditEvents(limit int) ([]nosql.AuditEvent, error) {
	if h.auditStore == nil {
		return []nosql.AuditEvent{}, nil
	}
	return h.auditStore.List(limit)
}

func (h *Handler) listAuditEventsFromRequest(r *http.Request) ([]nosql.AuditEvent, error) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return nil, errors.New("invalid limit")
		}
		limit = parsed
	}
	return h.listAuditEvents(limit)
}

func (h *Handler) recordAudit(r *http.Request, action, object, result, detail string) {
	h.recordAuditAt(r, time.Now().UTC(), action, object, result, detail)
}

// recordAuditAt 以调用方给定的时间戳写入审计。configTxn 在持有 configMu 时取得时间戳，
// 锁外才写入，使审计顺序与配置提交顺序一致。
func (h *Handler) recordAuditAt(r *http.Request, at time.Time, action, object, result, detail string) {
	if h.auditStore == nil {
		return
	}
	actor := "system"
	if session, ok := SessionFromContext(r.Context()); ok && session != nil && strings.TrimSpace(session.Username) != "" {
		actor = session.Username
	}
	event := nosql.AuditEvent{
		Timestamp: at,
		Actor:     actor,
		Action:    action,
		Object:    object,
		Result:    result,
		Detail:    detail,
	}
	if h.auditWriteHook != nil {
		h.auditWriteHook(event)
	}
	_ = h.auditStore.Record(event)
}
