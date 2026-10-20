// Admin handlers for embedded admin HTML and JavaScript serving.
package admin

import (
	"net/http"
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/version"
	"github.com/go-chi/chi/v5"
)

func (h *Handler) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	// 将构建期注入的版本号写入页面（占位符 __APP_VERSION__）。
	html := strings.Replace(adminUIHTML, "__APP_VERSION__", version.Version, 1)
	_, _ = w.Write([]byte(html))
}

func (h *Handler) handleUIJS(w http.ResponseWriter, r *http.Request) {
	file := chi.URLParam(r, "file")
	if !strings.HasSuffix(file, ".js") || strings.ContainsAny(file, "/\\") {
		http.NotFound(w, r)
		return
	}
	data, err := adminUIJSFS.ReadFile("ui/js/" + file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	_, _ = w.Write(data)
}
