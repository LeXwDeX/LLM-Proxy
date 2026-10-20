package admin

import (
	"regexp"
	"strings"
	"testing"
)

func TestUIModulesHaveExplicitStartupOrder(t *testing.T) {
	names := []string{"core", "schema", "navigation", "overview", "targets", "pools", "clients", "pricing", "usage", "audit_trace", "users", "catalog", "quota", "bootstrap"}
	last := -1
	for _, name := range names {
		path := "/admin/ui/js/" + name + ".js"
		pos := strings.Index(adminUIHTML, `src="`+path+`"`)
		if pos < 0 || pos <= last {
			t.Errorf("missing or out of order UI module %s", name)
		}
		last = pos
	}
	for _, src := range extractInlineScripts(adminUIHTML) {
		if strings.TrimSpace(src) != "" {
			t.Error("business JavaScript must live in responsibility-specific modules")
		}
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
	re := regexp.MustCompile(`<script\s+src="/admin/(ui/js/[^"/]+)"[^>]*></script>`)
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
