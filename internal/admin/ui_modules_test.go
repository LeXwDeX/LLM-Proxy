package admin

import (
	"regexp"
	"strings"
	"testing"
)

func TestUIModulesHaveExplicitStartupOrder(t *testing.T) {
	names := []string{"core/core", "core/schema", "core/navigation", "pages/overview/overview", "pages/targets/targets", "pages/pools/pools", "pages/clients/clients", "pages/pricing/pricing", "pages/usage/usage", "pages/audit_trace/audit_trace", "pages/users/users", "pages/catalog/catalog", "pages/quota/quota", "core/bootstrap"}
	last := -1
	for _, name := range names {
		path := "/admin/ui/js/" + name + ".js"
		pos := strings.Index(adminUIHTML, `src="`+path+`"`)
		if pos < 0 || pos <= last {
			t.Errorf("missing or out of order UI module %s", name)
		}
		last = pos
	}
	// The only inline script allowed is the dependency-free boot watchdog.
	inline := 0
	for _, src := range extractInlineScripts(adminUIHTML) {
		if strings.TrimSpace(src) != "" {
			inline++
			if !strings.Contains(src, "页面资源加载失败") || strings.Contains(src, "/admin/data/") {
				t.Error("business JavaScript must live in responsibility-specific modules")
			}
		}
	}
	if inline != 1 {
		t.Errorf("inline scripts = %d, want only the boot watchdog", inline)
	}
}

func TestUIClientPickerHasOneDefinition(t *testing.T) {
	source := adminUIHTML
	// Before extraction this regression also detects duplicate definitions in the inline source.
	for _, src := range adminUIJSSources() {
		source += "\n" + src
	}
	for _, name := range []string{"ensureClientPickerData", "renderClientTargetTags", "applyTagStyle", "getSelectedClientTargets", "renderClientPoolTags", "getSelectedClientPools"} {
		matches := regexp.MustCompile(`function\s+`+name+`\s*\(`).FindAllString(source, -1)
		if len(matches) != 1 {
			t.Errorf("%s has %d definitions, want one", name, len(matches))
		}
	}
}

func TestUIModuleManifestMatchesEmbeddedAssets(t *testing.T) {
	sources := adminUIJSSources()
	seen := map[string]bool{}
	re := regexp.MustCompile(`<script\s+src="/admin/(ui/js/(?:core|pages/[a-z_]+)/[a-z_]+\.js)(?:\?[^"]*)?"[^>]*></script>`)
	for _, match := range re.FindAllStringSubmatch(adminUIHTML, -1) {
		name := match[1]
		if seen[name] {
			t.Errorf("script loaded twice: %s", name)
		}
		seen[name] = true
		if _, ok := sources[name]; !ok {
			t.Errorf("manifest script is not embedded: %s", name)
		}
	}
	for name := range sources {
		if !seen[name] {
			t.Errorf("embedded script is not loaded by the manifest: %s", name)
		}
	}
}
