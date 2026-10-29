// Admin handlers for embedded admin HTML and JavaScript serving.
package admin

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/version"
	"github.com/go-chi/chi/v5"
)

// uiScriptSrcRe matches the module <script> tags in index.html. The served URL
// gets ?v=<version>-<nonce> so no browser cache entry (including a stale 302
// stored while logged out) can be reused for a new page load.
var uiScriptSrcRe = regexp.MustCompile(`(<script src="/admin/ui/js/[A-Za-z0-9_]+\.js)("></script>)`)

func versionScriptURLs(html string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is practically impossible; fall back to a zero
		// nonce (the version suffix and no-store headers still apply).
		buf = make([]byte, 8)
	}
	suffix := "?v=" + version.Version + "-" + hex.EncodeToString(buf)
	return uiScriptSrcRe.ReplaceAllString(html, "${1}"+suffix+"${2}")
}

func (h *Handler) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	setHardenedHeaders(w.Header())
	// 将构建期注入的版本号写入页面（占位符 __APP_VERSION__）。
	html := strings.Replace(adminUIHTML, "__APP_VERSION__", version.Version, 1)
	_, _ = w.Write([]byte(versionScriptURLs(html)))
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
	setHardenedHeaders(w.Header())
	_, _ = w.Write(data)
}
