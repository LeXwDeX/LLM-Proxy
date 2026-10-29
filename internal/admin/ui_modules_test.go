package admin

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestUIModulesHaveExplicitStartupOrder(t *testing.T) {
	// The script list is derived from the HTML; only the ordering contract is
	// fixed: the foundation loads first, schema/navigation before any feature
	// module, and bootstrap last.
	manifest := adminUIScriptManifest()
	if len(manifest) < 14 {
		t.Fatalf("script manifest has %d entries, want at least 14: %v", len(manifest), manifest)
	}
	if manifest[0] != "core/core.js" || manifest[1] != "core/schema.js" || manifest[2] != "core/navigation.js" {
		t.Errorf("startup scripts must begin core, schema, navigation; got %v", manifest[:3])
	}
	if manifest[len(manifest)-1] != "core/bootstrap.js" {
		t.Errorf("bootstrap.js must load last; got %s", manifest[len(manifest)-1])
	}
	for _, rel := range manifest[3 : len(manifest)-1] {
		if !strings.HasPrefix(rel, "pages/") {
			t.Errorf("feature module %s must live under pages/", rel)
		}
	}
	for _, page := range []string{"overview", "targets", "pools", "clients", "pricing", "usage", "audit_trace", "users", "catalog", "quota"} {
		prefix := "pages/" + page + "/"
		found := false
		for _, rel := range manifest {
			found = found || strings.HasPrefix(rel, prefix)
		}
		if !found {
			t.Errorf("page %s has no script in the manifest", page)
		}
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
	loose := regexp.MustCompile(`<script\s+src="/admin/ui/js/[^"?]+`)
	if got, want := len(loose.FindAllString(adminUIHTML, -1)), len(adminUIScriptManifest()); got != want {
		t.Fatalf("%d admin script tags but %d parse as .js with a clean path", got, want)
	}
	seen := map[string]int{}
	for _, rel := range adminUIScriptManifest() {
		seen[rel]++
		if !fs.ValidPath(rel) || strings.Contains(rel, "\\") {
			t.Errorf("script path %q is not a clean relative path", rel)
		}
		if _, ok := sources["ui/js/"+rel]; !ok {
			t.Errorf("manifest script does not resolve to an embedded file: %s", rel)
		}
	}
	for rel, n := range seen {
		if n != 1 {
			t.Errorf("script loaded %d times: %s", n, rel)
		}
	}
	for name := range sources {
		if seen[strings.TrimPrefix(name, "ui/js/")] != 1 {
			t.Errorf("embedded script is not loaded by exactly one tag: %s", name)
		}
	}
}
