package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-chi/chi/v5"
)

// TestAssembledHTMLMatchesNodeAssembly keeps the Go and Node include
// implementations identical; Node tests rely on the latter.
func TestAssembledHTMLMatchesNodeAssembly(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "-e",
		`process.stdout.write(require("../../scripts/testing/assemble_admin_html.cjs").assembleAdminHTML())`).Output()
	if err != nil {
		t.Fatalf("node assembly failed: %v", err)
	}
	if got := adminUIHTML; got != string(out) {
		t.Fatalf("Go and Node assembly differ (len %d vs %d)", len(got), len(out))
	}
}

func TestAssembleAdminHTMLFailsLoudly(t *testing.T) {
	build := func(files map[string]string) error {
		m := fstest.MapFS{}
		for k, v := range files {
			m["ui/"+k] = &fstest.MapFile{Data: []byte(v)}
		}
		_, err := assembleAdminHTML(m, "ui")
		return err
	}
	if err := build(map[string]string{"index.html": "a\n  <!-- @include p/x.html -->\nb\n", "p/x.html": "X\n"}); err != nil {
		t.Fatalf("valid include rejected: %v", err)
	}
	cases := map[string]map[string]string{
		"missing target":    {"index.html": "<!-- @include nope.html -->\n"},
		"cycle":             {"index.html": "<!-- @include a.html -->\n", "a.html": "<!-- @include b.html -->\n", "b.html": "<!-- @include a.html -->\n"},
		"inline marker":     {"index.html": "x <!-- @include a.html -->\n", "a.html": "A\n"},
		"escaping path":     {"index.html": "<!-- @include ../x.html -->\n"},
		"duplicate include": {"index.html": "<!-- @include a.html -->\n<!-- @include a.html -->\n", "a.html": "A\n"},
		"bom partial":       {"index.html": "<!-- @include a.html -->\n", "a.html": "\uFEFFA\n"},
		"bom index":         {"index.html": "\uFEFFx\n"},
		"no final newline":  {"index.html": "<!-- @include a.html -->\n", "a.html": "A"},
	}
	for name, files := range cases {
		if err := build(files); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	got, _ := assembleAdminHTML(fstest.MapFS{
		"ui/index.html": {Data: []byte("a\n  <!-- @include x.html -->\nb\n")},
		"ui/x.html":     {Data: []byte("  X\n")},
	}, "ui")
	if got != "a\n  X\nb\n" {
		t.Errorf("verbatim replacement broken: %q", got)
	}
}

func TestAdminUIHasNoLeftoverIncludes(t *testing.T) {
	if strings.Contains(adminUIHTML, uiIncludeMarker) {
		t.Fatal("assembled HTML still contains an include marker")
	}
}

func readPartials(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := adminUIFS.ReadDir("ui/partials/" + dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no partials in %s: %v", dir, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		data, err := adminUIFS.ReadFile("ui/partials/" + dir + "/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

// topLevelBlocks counts how many times the div depth returns to zero.
func topLevelBlocks(src string) int {
	re := regexp.MustCompile(`<div\b|</div>`)
	depth, blocks := 0, 0
	for _, m := range re.FindAllString(htmlCommentRe.ReplaceAllString(src, ""), -1) {
		if m == "</div>" {
			depth--
			if depth == 0 {
				blocks++
			}
		} else {
			depth++
		}
	}
	if depth != 0 {
		return -1
	}
	return blocks
}

func TestModalPartialsHoldExactlyOneOverlay(t *testing.T) {
	overlayRe := regexp.MustCompile(`(?m)^\s*<div class="(?:modal-overlay|quota-drawer-overlay)"`)
	for name, src := range readPartials(t, "modals") {
		if got := topLevelBlocks(src); got != 1 {
			t.Errorf("modals/%s: top-level div blocks = %d, want 1", name, got)
		}
		if !overlayRe.MatchString(src) {
			t.Errorf("modals/%s: top-level block is not an overlay", name)
		}
		if got := len(regexp.MustCompile(`<div class="(?:modal-overlay|quota-drawer-overlay)"[^>]*\bid=`).FindAllString(src, -1)); got != 1 {
			t.Errorf("modals/%s: overlays = %d, want 1", name, got)
		}
	}
}

func TestPagePartialsHoldExactlyOnePage(t *testing.T) {
	for name, src := range readPartials(t, "pages") {
		if got := topLevelBlocks(src); got != 1 {
			t.Errorf("pages/%s: top-level div blocks = %d, want 1", name, got)
		}
		if got := len(regexp.MustCompile(`<div class="page[ "]`).FindAllString(src, -1)); got != 1 {
			t.Errorf("pages/%s: div.page count = %d, want 1", name, got)
		}
	}
}

func TestHandleUIJSServesNestedPathsOnly(t *testing.T) {
	h := &Handler{}
	r := chi.NewRouter()
	r.Get("/ui/js/*", h.handleUIJS)
	status := func(path string) int {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code
	}
	for _, ok := range []string{"/ui/js/core/core.js", "/ui/js/pages/quota/quota.js?v=1"} {
		if got := status(ok); got != http.StatusOK {
			t.Errorf("%s = %d, want 200", ok, got)
		}
	}
	for _, bad := range []string{
		"/ui/js/core.js",                     // old flat path no longer exists
		"/ui/js/../index.html",               // non-js and traversal
		"/ui/js/core/../core/core.js",        // unclean
		"/ui/js/pages/../../js/core/core.js", // traversal
		"/ui/js//core/core.js",               // absolute-looking
		"/ui/js/core%5Ccore.js",              // backslash
		"/ui/js/core\\core.js",               // backslash
		"/ui/js/core/core.test.cjs",          // not .js
		"/ui/js/core/missing.js",
		"/ui/js/core/",
		"/ui/js/%2e%2e/index.html",
		"/ui/js/core/%2e%2e/core/core.js",
		"/ui/js/%2E%2E%2Fcore/core.js",
		"/ui/js/core%2Fcore.js",
		"/ui/js/%2Fcore/core.js",
	} {
		if got := status(bad); got != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", bad, got)
		}
	}
}

func TestUINodeAssemblyHelperMatchesGolden(t *testing.T) {
	runAdminNodeTest(t, "ui/assemble.test.cjs")
}
