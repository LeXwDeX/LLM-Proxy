package admin

import (
	"github.com/go-chi/chi/v5"
	"net/http"
	"strconv"
)

// handleGetTrace 按 request_id 查询单条 trace 记录。
// 仅供 Admin UI 内部使用，不对外暴露 API。
func (h *Handler) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	// 检查请求来源，确保来自 Admin UI
	if !isAdminUIRequest(r) {
		writeJSON(w, http.StatusForbidden, errorResponse("access denied"))
		return
	}

	requestID := chi.URLParam(r, "request_id")
	if requestID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("request_id required"))
		return
	}

	record := h.proxyService.GetTrace(requestID)
	if record == nil {
		writeJSON(w, http.StatusNotFound, errorResponse("trace not found"))
		return
	}

	writeJSON(w, http.StatusOK, record)
}

// handleListTrace 列出最近的 trace 记录，支持 offset 分页。
// 仅供 Admin UI 内部使用，不对外暴露 API。
func (h *Handler) handleListTrace(w http.ResponseWriter, r *http.Request) {
	// 检查请求来源，确保来自 Admin UI
	if !isAdminUIRequest(r) {
		writeJSON(w, http.StatusForbidden, errorResponse("access denied"))
		return
	}

	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}

	offset := 0
	if o := r.URL.Query().Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offset = n
		}
	}

	records := h.proxyService.ListTrace(offset, limit)
	stats := h.proxyService.TraceStats()

	writeJSON(w, http.StatusOK, map[string]any{
		"records": records,
		"count":   len(records),
		"stats":   stats,
	})
}

// isAdminUIRequest 检查请求是否来自 Admin UI。
// 通过检查 X-Requested-With 头来判断（UI 的 api() 函数会发送此头）。
func isAdminUIRequest(r *http.Request) bool {
	return r.Header.Get("X-Requested-With") == "XMLHttpRequest"
}
