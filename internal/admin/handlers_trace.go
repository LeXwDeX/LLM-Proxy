package admin

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/tracestore"
)

const (
	traceDefaultLimit = 50
	traceMaxLimit     = 5000
)

// handleTraceStats 只报告 trace store 是否启用及计数器：不读取记录、不扫描磁盘，
// 供外壳启动时低成本探测 Trace 页面是否可用。
func (h *Handler) handleTraceStats(w http.ResponseWriter, r *http.Request) {
	if !isAdminUIRequest(r) {
		writeJSON(w, http.StatusForbidden, errorResponse("access denied"))
		return
	}
	enabled := h.proxyService.TraceEnabled()
	response := map[string]any{"enabled": enabled}
	if enabled {
		response["stats"] = h.proxyService.TraceStats()
	}
	writeJSON(w, http.StatusOK, response)
}

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
	// Stored client-key masks may predate the suffix-only policy. Non-admin
	// readers get the viewer-safe form: the current 8-byte "****"+suffix mask is
	// kept, while legacy, raw-looking and unknown values are fully hidden. Copy
	// first: the record may still be owned by the in-memory ring buffer.
	if !isAdminSession(r) {
		record = redactTraceRecordForViewer(record)
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

	limit := traceDefaultLimit
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= traceMaxLimit {
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

	// limit/offset 回显服务端实际采用的值（非法或超限的 limit 回落到默认值），
	// 前端据此翻页；limit_max 让前端能预先限制输入并解释服务端的实际取值。
	// view=summary 返回不含 body/header 的列表投影；默认保持完整记录，
	// 但非管理员默认使用 summary，避免完整记录随列表下发。显式 view=full 仍允许，
	// 但非管理员的客户端密钥字段使用查看者安全形式：当前 "****"+后 4 位保留，
	// 遗留、原始与未知形态完全隐藏（旧遮罩无法证明原密钥长度）。
	view := r.URL.Query().Get("view")
	summary := view == "summary"
	if view == "" && !isAdminSession(r) {
		summary = true
	}
	response := map[string]any{
		"enabled":   h.proxyService.TraceEnabled(),
		"count":     len(records),
		"limit":     limit,
		"limit_max": traceMaxLimit,
		"offset":    offset,
		"stats":     stats,
	}
	if summary {
		response["view"] = "summary"
		response["records"] = tracestore.Summaries(records)
	} else {
		if !isAdminSession(r) {
			records = redactTraceRecordsForViewer(records)
		}
		if records == nil {
			response["records"] = []*tracestore.TraceRecord{}
		} else {
			response["records"] = records
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// redactTraceRecordForViewer copies a record and normalizes its client-key
// mask. Trace records may be owned by the store's ring buffer, so the caller's
// view must never mutate the original.
func redactTraceRecordForViewer(record *tracestore.TraceRecord) *tracestore.TraceRecord {
	if record == nil {
		return nil
	}
	clone := *record
	clone.ClientAccessKey = auth.NormalizeClientKeyMask(record.ClientAccessKey)
	return &clone
}

func redactTraceRecordsForViewer(records []*tracestore.TraceRecord) []*tracestore.TraceRecord {
	if records == nil {
		return nil
	}
	out := make([]*tracestore.TraceRecord, len(records))
	for i, record := range records {
		out[i] = redactTraceRecordForViewer(record)
	}
	return out
}

// isAdminUIRequest 检查请求是否来自 Admin UI。
// 通过检查 X-Requested-With 头来判断（UI 的 api() 函数会发送此头）。
func isAdminUIRequest(r *http.Request) bool {
	return r.Header.Get("X-Requested-With") == "XMLHttpRequest"
}
