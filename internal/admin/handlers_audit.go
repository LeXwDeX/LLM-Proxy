package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
)

func (h *Handler) handleListAudit(w http.ResponseWriter, r *http.Request) {
	events, err := h.listAuditEventsFromRequest(r)
	if err != nil {
		h.writeInternalError(w, "failed to list audit events", err)
		return
	}
	events = redactAuditEventsForViewer(r, events)
	writeJSON(w, http.StatusOK, map[string]any{
		"events": events,
		"count":  len(events),
	})
}

// clientKeyAuditActions lists the audit actions whose Detail is a client-key
// mask. Other actions keep their details untouched.
var clientKeyAuditActions = map[string]struct{}{
	"client_create": {},
	"client_update": {},
}

// redactAuditEventForViewer normalizes a stored client-key mask to the
// viewer-safe form. Unknown and legacy values are fully hidden rather than
// echoed.
func redactAuditEventForViewer(event nosql.AuditEvent) nosql.AuditEvent {
	if _, ok := clientKeyAuditActions[event.Action]; !ok || event.Detail == "" {
		return event
	}
	event.Detail = auth.NormalizeClientKeyMask(event.Detail)
	return event
}

// redactAuditEventsForViewer applies the viewer role to a freshly loaded audit
// list. Admins keep the stored values for compatibility; every other role only
// sees normalized client-key details. Both the audit list and the overview
// recent-audit projection must go through this helper.
func redactAuditEventsForViewer(r *http.Request, events []nosql.AuditEvent) []nosql.AuditEvent {
	if isAdminSession(r) {
		return events
	}
	for i := range events {
		events[i] = redactAuditEventForViewer(events[i])
	}
	return events
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
