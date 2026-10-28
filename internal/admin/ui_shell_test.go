package admin

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// uiCSSBlock returns the declaration body of the first exact selector block.
func uiCSSBlock(t *testing.T, selector string) string {
	t.Helper()
	re := regexp.MustCompile(`(?ms)^[ \t]*` + regexp.QuoteMeta(selector) + `[ \t]*\{(.*?)\}`)
	match := re.FindStringSubmatch(adminUIHTML)
	if match == nil {
		t.Fatalf("index.html 缺少 CSS 选择器 %s", selector)
	}
	return match[1]
}

func uiCSSValue(t *testing.T, block, name string) string {
	t.Helper()
	re := regexp.MustCompile(`--` + regexp.QuoteMeta(name) + `\s*:\s*([^;]+);`)
	match := re.FindStringSubmatch(block)
	if match == nil {
		t.Fatalf("缺少 token --%s", name)
	}
	return strings.TrimSpace(match[1])
}

// The prototype defines these exact values. Keep the casing so an accidental
// formatting pass cannot silently replace the approved palette.
func TestUIShellDefinesPrototypeThemeTokens(t *testing.T) {
	light := map[string]string{
		"bg": "#F4F5F7", "surface": "#FFFFFF", "surface-2": "#F8F9FB",
		"line": "#E3E6EB", "line-2": "#EEF0F3", "ink": "#14171C",
		"ink-2": "#444C59", "ink-3": "#5C6573", "accent": "#2B59D9",
		"accent-ink": "#FFFFFF", "accent-soft": "#E9EEFC", "out": "#A65314",
		"out-soft": "#FBEEE2", "ok": "#1C7A4A", "ok-soft": "#E2F2E9",
		"warn": "#8A5700", "warn-soft": "#FAF0DA", "bad": "#B42318",
		"bad-soft": "#FCE8E6", "muted-soft": "#EEF0F3", "side": "#11141A",
		"side-2": "#1D222B", "side-ink": "#D3D8DF", "side-ink-2": "#8E97A4",
	}
	dark := map[string]string{
		"bg": "#0D0F12", "surface": "#15181D", "surface-2": "#1A1E24",
		"line": "#272C34", "line-2": "#20252C", "ink": "#E7EAEE",
		"ink-2": "#B9C0CA", "ink-3": "#959EAB", "accent": "#7D9CFF",
		"accent-ink": "#0D0F12", "accent-soft": "#1C2645", "out": "#F0A15C",
		"out-soft": "#3A2715", "ok": "#5CC48C", "ok-soft": "#14301F",
		"warn": "#E6B450", "warn-soft": "#33270F", "bad": "#F2796D",
		"bad-soft": "#3A1916", "muted-soft": "#22272E", "side": "#090B0E",
		"side-2": "#181C23",
	}
	for name, want := range light {
		if got := uiCSSValue(t, uiCSSBlock(t, ".app"), name); got != want {
			t.Errorf("亮色 --%s = %q, want %q", name, got, want)
		}
	}
	for name, want := range dark {
		if got := uiCSSValue(t, uiCSSBlock(t, ".app.dark"), name); got != want {
			t.Errorf("暗色 --%s = %q, want %q", name, got, want)
		}
	}
	app := uiCSSBlock(t, ".app")
	if !strings.Contains(app, "color:var(--ink)") || !strings.Contains(app, "background:var(--bg)") {
		t.Errorf(".app 必须使用 --ink/--bg 呈现主题表面")
	}
	if !regexp.MustCompile(`<html[^>]*class="app"`).MatchString(adminUIHTML) {
		t.Errorf("<html> 必须带 class=\"app\"，让全局 token 覆盖整页")
	}
}

// Issue #55: one platform-ordered system font stack, shared through tokens.
// CJK faces are listed per platform (macOS, then Windows, then Linux) ahead of
// the Latin fallbacks so Chinese text never falls through to a Latin face.
const (
	wantFontSans = `-apple-system, BlinkMacSystemFont, "SF Pro Text", "PingFang SC", "Hiragino Sans GB", "Segoe UI", "Microsoft YaHei UI", "Microsoft YaHei", "Noto Sans SC", "Source Han Sans SC", Roboto, "Helvetica Neue", Arial, sans-serif`
	wantFontMono = `ui-monospace, "SF Mono", SFMono-Regular, Menlo, "Cascadia Mono", Consolas, "Liberation Mono", monospace`
)

func TestUIShellUsesSystemFontsAndTabularNumbers(t *testing.T) {
	root := uiCSSBlock(t, ":root")
	if got := uiCSSValue(t, root, "font-sans"); got != wantFontSans {
		t.Errorf("--font-sans = %s\nwant %s", got, wantFontSans)
	}
	if got := uiCSSValue(t, root, "font-mono"); got != wantFontMono {
		t.Errorf("--font-mono = %s\nwant %s", got, wantFontMono)
	}
	if app := uiCSSBlock(t, ".app"); !regexp.MustCompile(`font-family:\s*var\(--font-sans\)`).MatchString(app) {
		t.Errorf(".app 正文字体必须引用 var(--font-sans)")
	}
	body := uiCSSBlock(t, "body")
	for _, want := range []string{"-webkit-font-smoothing: antialiased", "-moz-osx-font-smoothing: grayscale", "text-rendering: optimizeLegibility"} {
		if !strings.Contains(body, want) {
			t.Errorf("body 缺少字体渲染设置 %q", want)
		}
	}
	mono := uiCSSBlock(t, ".mono")
	for _, want := range []string{"var(--font-mono)", "font-variant-numeric:tabular-nums"} {
		if !strings.Contains(mono, want) {
			t.Errorf(".mono 缺少 %q", want)
		}
	}
	// Only local system faces: no web font loading of any kind.
	for _, banned := range []string{"@font-face", "IBM Plex", "fonts.googleapis", "fonts.gstatic"} {
		if strings.Contains(adminUIHTML, banned) {
			t.Errorf("管理台不得依赖外部字体或 CDN：发现 %q", banned)
		}
	}
}

// Every font-family declaration (CSS, inline styles, JS-rendered markup) uses
// the tokens or inherit; scattered literal stacks drift apart over time.
func TestUIFontFamiliesUseTokens(t *testing.T) {
	decl := regexp.MustCompile(`font-family\s*:\s*([^;'"}]+)`)
	allowed := regexp.MustCompile(`^(var\(--font-(sans|mono)\)|inherit)\s*$`)
	sources := map[string]string{"index.html": adminUIHTML}
	entries, err := adminUIJSFS.ReadDir("ui/js")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := adminUIJSFS.ReadFile("ui/js/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		sources[entry.Name()] = string(data)
	}
	for name, source := range sources {
		for _, match := range decl.FindAllStringSubmatch(source, -1) {
			if !allowed.MatchString(match[1]) {
				t.Errorf("%s: font-family 必须使用 var(--font-sans)/var(--font-mono)/inherit，发现 %q", name, strings.TrimSpace(match[0]))
			}
		}
	}
}

// Issue #55: on ultra-wide screens percentage name/key columns spread sparse
// content; they are bounded so the model list column absorbs the remainder.
func TestUITargetsTableBoundsNameAndKeyColumns(t *testing.T) {
	px := regexp.MustCompile(`width:\s*(\d+)px`)
	name := uiCSSBlock(t, ".targets-table .col-name")
	keys := uiCSSBlock(t, ".targets-table .col-keys")
	for label, block := range map[string]string{"col-name": name, "col-keys": keys} {
		if strings.Contains(block, "%") {
			t.Errorf("%s 不得使用百分比宽度: %s", label, block)
		}
	}
	if !strings.Contains(name, "clamp(") && !px.MatchString(name) {
		t.Errorf("col-name 必须使用 clamp() 或固定 px 宽度: %s", name)
	}
	// Keys hold up to ~4 status chips on one line: clamp(180px, 13vw, 240px).
	clamp := regexp.MustCompile(`width:\s*clamp\((\d+)px,\s*[\d.]+vw,\s*(\d+)px\)`).FindStringSubmatch(keys)
	if clamp == nil {
		t.Fatalf("col-keys 必须使用 clamp(<min>px, <n>vw, <max>px): %s", keys)
	}
	if lo, _ := strconv.Atoi(clamp[1]); lo < 180 {
		t.Errorf("col-keys 最小宽度应 ≥180px，实际 %dpx", lo)
	}
	if hi, _ := strconv.Atoi(clamp[2]); hi > 240 {
		t.Errorf("col-keys 最大宽度应 ≤240px，实际 %dpx", hi)
	}
	if !strings.Contains(uiCSSBlock(t, ".targets-table"), "min-width:1360px") {
		t.Errorf("目标表格须保留 min-width:1360px，窄屏横向滚动行为不变")
	}
	if !strings.Contains(adminUIHTML, `<col class="col-scheduling"><col><col class="col-actions">`) {
		t.Errorf("模型列表列须保持无宽度 <col>，以吸收剩余宽度")
	}
}

// With table-layout:fixed the unclassed model-list column only receives what
// the bounded columns leave over. Near a 1600px viewport they fill the
// container, the model list collapses to its padding and the barely
// overflowing table gives the scrollbar nothing to reveal. The table minimum
// must therefore cover every bounded column plus a model-list minimum.
func TestUITargetsTableReservesModelListMinimum(t *testing.T) {
	lastWidth := regexp.MustCompile(`(?:^|[;{\s])width:\s*([^;]+);`)
	table := uiCSSBlock(t, ".targets-table")
	calc := regexp.MustCompile(`min-width:\s*calc\(([^;]+)\);`).FindStringSubmatch(table)
	if calc == nil {
		t.Fatalf(".targets-table 须用 calc() 声明覆盖全部列的 min-width: %s", table)
	}
	// One term per column in colgroup order; the model list sits before actions.
	var want []string
	for _, col := range []string{"status", "name", "keys", "type", "pool", "scheduling", "", "actions"} {
		if col == "" {
			want = append(want, "var(--targets-model-min)")
			continue
		}
		block := uiCSSBlock(t, ".targets-table .col-"+col)
		widths := lastWidth.FindAllStringSubmatch(block, -1)
		if len(widths) == 0 {
			t.Fatalf("col-%s 缺少宽度: %s", col, block)
		}
		want = append(want, strings.TrimSpace(widths[len(widths)-1][1]))
	}
	var got []string
	for _, term := range strings.Split(calc[1], " + ") {
		got = append(got, strings.TrimSpace(term))
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("min-width 须按列顺序逐项覆盖实际宽度:\n got  %s\n want %s", strings.Join(got, " | "), strings.Join(want, " | "))
	}
	model := regexp.MustCompile(`--targets-model-min:\s*(\d+)px;`).FindStringSubmatch(table)
	if model == nil {
		t.Fatalf("min-width 须为模型列表列保留 --targets-model-min: %s", table)
	}
	if px, _ := strconv.Atoi(model[1]); px < 200 {
		t.Errorf("模型列表最小宽度应 ≥200px，实际 %dpx", px)
	}
}

// The login portal is a standalone page; it inlines the same stack and rendering.
func TestLoginPageUsesSharedFontStack(t *testing.T) {
	portal, _ := newPortalTestFixture(t)
	body := renderPortalLoginPage(t, portal, "/login").Body.String()
	if !strings.Contains(body, wantFontSans) {
		t.Errorf("登录页字体栈须与管理台 --font-sans 一致")
	}
	if ping, segoe := strings.Index(body, `"PingFang SC"`), strings.Index(body, `"Segoe UI"`); ping < 0 || segoe < 0 || ping > segoe {
		t.Errorf("登录页字体栈中 PingFang SC 须排在 Segoe UI 之前")
	}
	for _, want := range []string{"-webkit-font-smoothing:antialiased", "-moz-osx-font-smoothing:grayscale", "text-rendering:optimizeLegibility"} {
		if !strings.Contains(body, want) {
			t.Errorf("登录页缺少字体渲染设置 %q", want)
		}
	}
}

func uiSidebarNav(t *testing.T) string {
	t.Helper()
	asideStart := strings.Index(adminUIHTML, `<aside class="sidebar"`)
	if asideStart < 0 {
		t.Fatal("缺少侧栏")
	}
	aside := adminUIHTML[asideStart:]
	if end := strings.Index(aside, "</aside>"); end >= 0 {
		aside = aside[:end]
	}
	navStart := strings.Index(aside, "<nav")
	if navStart < 0 {
		t.Fatal("侧栏缺少主导航")
	}
	nav := aside[navStart:]
	if end := strings.Index(nav, "</nav>"); end >= 0 {
		nav = nav[:end]
	}
	return nav
}

func TestUISidebarGroupsAndInlineIcons(t *testing.T) {
	nav := uiSidebarNav(t)
	groups := []struct {
		label string
		pages []string
	}{
		{"首页", []string{"overview"}},
		{"上游与接入", []string{"targets", "clients"}},
		{"计费与额度", []string{"model-costs", "quota", "model-catalog"}},
		{"观测", []string{"usage", "audit", "trace"}},
		{"系统", []string{"users"}},
	}
	parts := strings.Split(nav, `class="nav-section-title"`)
	if len(parts)-1 != len(groups) {
		t.Fatalf("导航分组数量 = %d, want %d", len(parts)-1, len(groups))
	}
	for i, group := range groups {
		seg := parts[i+1]
		label := regexp.MustCompile(`^[^>]*>([^<]+)<`).FindStringSubmatch(seg)
		if label == nil || strings.TrimSpace(label[1]) != group.label {
			t.Errorf("第 %d 组标题 = %q, want %q", i+1, label, group.label)
		}
		matches := regexp.MustCompile(`data-page="([^"]+)"`).FindAllStringSubmatch(seg, -1)
		var pages []string
		for _, m := range matches {
			pages = append(pages, m[1])
		}
		if strings.Join(pages, ",") != strings.Join(group.pages, ",") {
			t.Errorf("%s 组页面 = %v, want %v", group.label, pages, group.pages)
		}
	}
	icons := regexp.MustCompile(`(?s)<span class="nav-icon">(.*?)</span>`).FindAllStringSubmatch(nav, -1)
	if len(icons) != 10 {
		t.Fatalf("导航图标数量 = %d, want 10", len(icons))
	}
	for i, icon := range icons {
		if !strings.HasPrefix(strings.TrimSpace(icon[1]), "<svg") {
			t.Errorf("第 %d 个导航图标不是内联 SVG", i+1)
		}
	}
	if !strings.Contains(nav, `id="nav-users"`) || !strings.Contains(nav, `id="nav-trace"`) {
		t.Error("必须保留 #nav-users 与 #nav-trace 供角色和 trace 开关使用")
	}
	if !strings.Contains(nav, `id="nav-section-system"`) {
		t.Error("必须保留 #nav-section-system 供 admin-only 分组开关使用")
	}
	if !strings.Contains(nav, `aria-current="page"`) {
		t.Error("当前导航项必须带 aria-current=\"page\"")
	}
	for _, item := range regexp.MustCompile(`class="sidebar-nav-item[^"]*"[^>]*`).FindAllString(nav, -1) {
		if !strings.Contains(item, `tabindex="0"`) || !strings.Contains(item, `role="link"`) {
			t.Errorf("导航项缺少键盘可达语义：%s", item)
		}
	}
}

// TestUISidebarFooterVersionAligned 锁定侧栏底部版本号的排版：底部留白，不贴窗口下沿；
// 版本文字与退出按钮的文字左对齐（按钮内边距 10px + 图标 18px + 间距 10px = 38px）。
func TestUISidebarFooterVersionAligned(t *testing.T) {
	footer := uiCSSBlock(t, ".sidebar-footer")
	if !strings.Contains(footer, "padding: 12px 12px 16px;") {
		t.Errorf(".sidebar-footer 底部须留白，避免版本号贴住窗口下沿：%s", footer)
	}
	version := uiCSSBlock(t, ".sidebar-version")
	if !strings.Contains(version, "padding: 0 10px 0 38px;") {
		t.Errorf(".sidebar-version 须与退出按钮文字左对齐（左内边距 38px）：%s", version)
	}
	button := uiCSSBlock(t, ".sidebar-footer .logout-btn")
	if !strings.Contains(button, "padding: 0 10px;") || !strings.Contains(button, "gap: 10px;") {
		t.Errorf("退出按钮内边距或图标间距变化后须同步 .sidebar-version 左内边距：%s", button)
	}
	if !strings.Contains(adminUIHTML, `<div class="sidebar-version mono">版本 __APP_VERSION__</div>`) {
		t.Error("侧栏须保留 __APP_VERSION__ 占位符，由 handlers_ui.go 在响应时替换")
	}
}

func TestUIShellHasNoExternalResources(t *testing.T) {
	for _, banned := range []string{"fonts.googleapis", "fonts.gstatic", "@import", "<link"} {
		if strings.Contains(adminUIHTML, banned) {
			t.Errorf("管理台不得请求外部资源：发现 %q", banned)
		}
	}
	for _, pattern := range []string{`(?i)src\s*=\s*["']https?://`, `(?i)href\s*=\s*["']https?://`, `(?i)url\(\s*["']?https?://`} {
		if regexp.MustCompile(pattern).MatchString(adminUIHTML) {
			t.Errorf("管理台不得从外部 URL 加载资源：%s", pattern)
		}
	}
}

func TestUIIconButtonsHaveAccessibleNames(t *testing.T) {
	for _, id := range []string{"menuToggle", "themeToggle"} {
		tag := regexp.MustCompile(`(?s)<button\b[^>]*\bid="` + id + `"[^>]*>`).FindString(adminUIHTML)
		if tag == "" {
			t.Fatalf("缺少按钮 #%s", id)
		}
		if !strings.Contains(tag, "aria-label=") {
			t.Errorf("#%s 是图标按钮，必须有 aria-label", id)
		}
	}
	for _, tag := range regexp.MustCompile(`(?s)<button\b[^>]*class="modal-close"[^>]*>`).FindAllString(adminUIHTML, -1) {
		if !strings.Contains(tag, "aria-label=") {
			t.Errorf("弹层关闭按钮必须有 aria-label：%s", tag)
		}
	}
	theme := regexp.MustCompile(`(?s)<button\b[^>]*\bid="themeToggle"[^>]*>.*?</button>`).FindString(adminUIHTML)
	if !strings.Contains(theme, "theme-icon-sun") || !strings.Contains(theme, "theme-icon-moon") {
		t.Error("主题切换必须包含亮/暗两个内联图标")
	}
	if !strings.Contains(theme, `aria-pressed=`) {
		t.Error("主题切换必须暴露 aria-pressed 状态")
	}
}

func TestUIBadgesUseThemeTokens(t *testing.T) {
	checks := map[string][]string{
		".quota-badge.ok":               {"var(--ok-soft)", "var(--ok)"},
		".quota-badge.warn":             {"var(--warn-soft)", "var(--warn)"},
		".quota-badge.over":             {"var(--bad-soft)", "var(--bad)"},
		".quota-badge.none":             {"var(--muted-soft)", "var(--ink-3)"},
		".quota-badge.del":              {"var(--muted-soft)", "var(--ink-3)"},
		".model-cat-badge.cat-free":     {"var(--ok-soft)", "var(--ok)"},
		".model-cat-badge.cat-low":      {"var(--accent-soft)", "var(--accent)"},
		".model-cat-badge.cat-standard": {"var(--muted-soft)", "var(--ink-3)"},
		".model-cat-badge.cat-high":     {"var(--bad-soft)", "var(--bad)"},
		".debug-badge":                  {"var(--warn-soft)", "var(--warn)"},
		".readonly-badge":               {"var(--warn-soft)", "var(--warn)"},
		".client-target-direct-tag":     {"var(--accent-soft)", "var(--accent)"},
		".client-target-pool-tag":       {"var(--out-soft)", "var(--out)"},
	}
	for selector, wants := range checks {
		block := uiCSSBlock(t, selector)
		for _, want := range wants {
			if !strings.Contains(block, want) {
				t.Errorf("%s 必须使用主题 token %s", selector, want)
			}
		}
	}
	hex := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	for _, name := range []string{"targets.js", "pools.js", "quota.js", "schema.js"} {
		if got := hex.FindString(adminUIJSSources()["ui/js/"+name]); got != "" {
			t.Errorf("%s 仍含硬编码颜色 %s；动态徽章与状态色必须改用 CSS token", name, got)
		}
	}
}

func TestUIEndpointBadgesUseThemeTokensInDarkMode(t *testing.T) {
	schema := adminUIJSSources()["ui/js/schema.js"]
	styleFn := regexp.MustCompile(`(?s)function epBadgeStyle\(code\) \{.*?\n  \}`).FindString(schema)
	if styleFn == "" {
		t.Fatal("schema.js 缺少 epBadgeStyle")
	}
	for _, want := range []string{"--ep-bg:", "--ep-fg:", "badge_background", "badge_foreground", "var(--muted-soft)", "var(--ink-3)"} {
		if !strings.Contains(styleFn, want) {
			t.Errorf("epBadgeStyle 缺少 %q", want)
		}
	}
	for _, banned := range []string{"background:", "color:"} {
		if strings.Contains(styleFn, banned) {
			t.Errorf("epBadgeStyle 不得输出内联 %s，主题由 CSS class 决定", banned)
		}
	}

	catalog := adminUIJSSources()["ui/js/catalog.js"]
	badgeFn := regexp.MustCompile(`(?s)function epBadge\(ep\) \{.*?\n    \}`).FindString(catalog)
	if badgeFn == "" {
		t.Fatal("catalog.js 缺少 epBadge")
	}
	if !strings.Contains(badgeFn, "class='ep-badge'") {
		t.Error("epBadge 必须输出稳定的 ep-badge class")
	}
	for _, banned := range []string{"background:", "color:"} {
		if strings.Contains(badgeFn, banned) {
			t.Errorf("epBadge 不得输出内联 %s", banned)
		}
	}

	light := uiCSSBlock(t, ".ep-badge")
	if !strings.Contains(light, "var(--ep-bg") || !strings.Contains(light, "var(--ep-fg") {
		t.Error(".ep-badge 必须使用 --ep-bg/--ep-fg 并保留 token 回退")
	}
	dark := uiCSSBlock(t, ".app.dark .ep-badge")
	if !strings.Contains(dark, "background:var(--accent-soft)") || !strings.Contains(dark, "color:var(--accent)") {
		t.Error(".app.dark .ep-badge 必须用 --accent-soft/--accent 覆盖元数据颜色")
	}
	if regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`).MatchString(dark) {
		t.Error(".app.dark .ep-badge 不得硬编码颜色")
	}
}

func TestUIExpansionButtonsExposeAriaState(t *testing.T) {
	for _, name := range []string{"quota.js", "usage.js", "pricing.js"} {
		src := adminUIJSSources()["ui/js/"+name]
		idx := strings.Index(src, "quota-expand-btn")
		if idx < 0 {
			t.Fatalf("%s 缺少 quota-expand-btn 生成逻辑", name)
		}
		end := strings.Index(src[idx:], "</button>")
		if end < 0 {
			t.Fatalf("%s 展开按钮缺少结束标签", name)
		}
		button := src[idx : idx+end]
		for _, want := range []string{"aria-label='展开/收起明细'", "aria-expanded='", `(expanded ? "true" : "false")`} {
			if !strings.Contains(button, want) {
				t.Errorf("%s 展开按钮缺少 %s", name, want)
			}
		}
	}
	for _, name := range []string{"quota.js", "pricing.js"} {
		source := adminUIJSSources()["ui/js/"+name]
		idx := strings.Index(source, "quota-expand-btn")
		end := strings.Index(source[idx:], "</button>")
		if !strings.Contains(source[idx:idx+end], "aria-controls='") {
			t.Errorf("%s 展开按钮缺少 aria-controls", name)
		}
	}
	quotaSrc := adminUIJSSources()["ui/js/quota.js"]
	if !strings.Contains(quotaSrc, "quota-detail-row' id='") || !strings.Contains(quotaSrc, "esc(detailId)") {
		t.Error("quota 明细行必须用与 aria-controls 相同的 detailId")
	}
}

func TestUIThemePreferenceContract(t *testing.T) {
	core := adminUIJSSources()["ui/js/core.js"]
	for _, want := range []string{"localStorage", `classList.toggle("dark"`, "aria-pressed", "themeToggle"} {
		if !strings.Contains(core, want) {
			t.Errorf("core.js 主题逻辑缺少 %q", want)
		}
	}
	if strings.Contains(adminUIHTML, "<script>") {
		t.Error("主题初始化不得通过内联脚本实现，必须保持模块启动顺序")
	}
}

func TestUITraceBodyPreviewReadableInBothThemes(t *testing.T) {
	block := uiCSSBlock(t, ".trace-body-preview")
	if !strings.Contains(block, "background: var(--side)") {
		t.Error(".trace-body-preview 必须使用 --side 深色背景")
	}
	if !strings.Contains(block, "color: var(--side-ink)") {
		t.Error(".trace-body-preview 必须使用 --side-ink 前景，--line 在暗色主题下几乎不可读")
	}
	if strings.Contains(block, "color: var(--line)") {
		t.Error(".trace-body-preview 不得再用 --line 作为正文前景")
	}
}

func TestUIThemeToggleIconDirection(t *testing.T) {
	// DESIGN.md: light shows the moon (switch to dark), dark shows the sun.
	sun := uiCSSBlock(t, ".theme-icon-sun")
	moon := uiCSSBlock(t, ".theme-icon-moon")
	darkSun := uiCSSBlock(t, ".app.dark .theme-icon-sun")
	darkMoon := uiCSSBlock(t, ".app.dark .theme-icon-moon")
	if !strings.Contains(sun, "display: none") {
		t.Error("亮色主题必须隐藏太阳图标")
	}
	if !strings.Contains(moon, "display: block") {
		t.Error("亮色主题必须显示月亮图标")
	}
	if !strings.Contains(darkSun, "display: block") {
		t.Error("暗色主题必须显示太阳图标")
	}
	if !strings.Contains(darkMoon, "display: none") {
		t.Error("暗色主题必须隐藏月亮图标")
	}
}

func TestUIMobileSidebarIsolatesFocusWhenClosed(t *testing.T) {
	nav := adminUIJSSources()["ui/js/navigation.js"]
	for _, want := range []string{
		`matchMedia("(max-width: 768px)")`,
		"sidebar.inert = isolated",
		"sidebar.contains(document.activeElement)",
		"toggle.focus()",
		`setAttribute("aria-hidden"`,
	} {
		if !strings.Contains(nav, want) {
			t.Errorf("navigation.js 移动端侧栏缺少焦点隔离 %q", want)
		}
	}
	if !regexp.MustCompile(`(?s)function setSidebarOpen\(open\) \{.*?inert`).MatchString(nav) {
		t.Error("setSidebarOpen 必须根据打开状态切换 inert")
	}
}

func TestUIMobileNavFocusAndMenuLabel(t *testing.T) {
	nav := adminUIJSSources()["ui/js/navigation.js"]
	setOpen := regexp.MustCompile(`(?s)function setSidebarOpen\(open\) \{.*?\n  \}`).FindString(nav)
	if setOpen == "" {
		t.Fatal("navigation.js 缺少 setSidebarOpen")
	}
	if !strings.Contains(setOpen, "open && isMobileNav()") {
		t.Error("仅移动端打开抽屉时才移动焦点")
	}
	if !strings.Contains(setOpen, `sidebar.querySelector(".sidebar-nav-item")`) {
		t.Error("打开移动端抽屉必须定位第一个导航项")
	}
	if !strings.Contains(setOpen, "firstNav.focus()") {
		t.Error("打开移动端抽屉必须把焦点放到第一个导航项")
	}
	labelIdx := strings.Index(setOpen, `setAttribute("aria-label"`)
	if labelIdx < 0 {
		t.Fatal("菜单按钮缺少 aria-label 状态同步")
	}
	labelCall := setOpen[labelIdx:]
	if end := strings.Index(labelCall, ");"); end >= 0 {
		labelCall = labelCall[:end]
	}
	for _, want := range []string{"open ?", `"关闭主导航"`, `"打开主导航"`} {
		if !strings.Contains(labelCall, want) {
			t.Errorf("菜单按钮 aria-label 必须反映开合状态，缺少 %q", want)
		}
	}
	escapeIdx := strings.Index(nav, `event.key === "Escape"`)
	if escapeIdx < 0 {
		t.Fatal("移动端抽屉必须支持 Escape 关闭")
	}
	escapeBlock := nav[escapeIdx:]
	if end := strings.Index(escapeBlock, "\n  });"); end >= 0 {
		escapeBlock = escapeBlock[:end]
	}
	for _, want := range []string{"isMobileNav()", "classList.contains(\"open\")", "setSidebarOpen(false)"} {
		if !strings.Contains(escapeBlock, want) {
			t.Errorf("Escape 关闭契约缺少 %q", want)
		}
	}
}

func TestUIMainContentLandmark(t *testing.T) {
	main := regexp.MustCompile(`<main\b[^>]*\bid="mainContent"[^>]*>`).FindString(adminUIHTML)
	if main == "" {
		t.Fatal("管理内容必须使用带稳定 id 的 <main> landmark")
	}
	if !strings.Contains(main, `class="content"`) {
		t.Error("#mainContent 必须保留 .content 布局类")
	}
	if got := strings.Count(adminUIHTML, "<main"); got != 1 {
		t.Errorf("<main> 必须唯一，found %d", got)
	}
	if !strings.Contains(adminUIHTML, "</main>") {
		t.Error("<main> 缺少闭合标签")
	}
}

func TestUIFocusRingsAndHoverAliasesUseThemeTokens(t *testing.T) {
	for name, selector := range map[string]string{"亮色": ".app", "暗色": ".app.dark"} {
		block := uiCSSBlock(t, selector)
		accent := uiCSSValue(t, block, "accent")
		if hover := uiCSSValue(t, block, "primary-hover"); hover == accent || hover == "var(--accent)" {
			t.Errorf("%s --primary-hover 必须是与 --accent 不同的悬停色", name)
		}
		bad := uiCSSValue(t, block, "bad")
		if hover := uiCSSValue(t, block, "danger-hover"); hover == bad || hover == "var(--bad)" {
			t.Errorf("%s --danger-hover 必须是与危险色不同的悬停色", name)
		}
	}
	for _, name := range []string{"focus-ring", "focus-ring-side"} {
		if uiCSSValue(t, uiCSSBlock(t, ".app"), name) == "" {
			t.Errorf(".app 缺少焦点环 token --%s", name)
		}
	}
	sidebarFocus := uiCSSBlock(t, ".sidebar-nav-item:focus-visible")
	if !strings.Contains(sidebarFocus, "var(--focus-ring-side)") || strings.Contains(sidebarFocus, "#8FA9FF") {
		t.Error("侧栏焦点环必须使用 --focus-ring-side token，不得硬编码浅蓝")
	}
	formFocus := uiCSSBlock(t, ".form-control:focus")
	if !strings.Contains(formFocus, "var(--accent-soft)") || strings.Contains(formFocus, "rgba(") {
		t.Error("输入焦点外环必须使用 --accent-soft token，不得硬编码 rgba")
	}
	if !strings.Contains(uiCSSBlock(t, ".btn:focus-visible"), "var(--focus-ring)") {
		t.Error("按钮焦点环必须使用 --focus-ring token")
	}
}

func TestUIIconControlsMeetMinimumTargetSize(t *testing.T) {
	for _, selector := range []string{".menu-toggle", ".theme-toggle", ".modal-close", ".quota-expand-btn", "#page-overview .activity-info"} {
		block := uiCSSBlock(t, selector)
		for _, dimension := range []string{"width", "height"} {
			if !regexp.MustCompile(dimension + `\s*:\s*34px`).MatchString(block) {
				t.Errorf("%s 必须提供至少 34px 的点击尺寸（缺少 %s:34px）", selector, dimension)
			}
		}
	}
	if strings.Contains(adminUIHTML, ".theme-toggle { width: 32px; height: 32px; }") {
		t.Error("窄屏不得把主题按钮缩小到 34px 以下")
	}
}

func TestUIAdminTablesUseScrollContainers(t *testing.T) {
	wrapped := regexp.MustCompile(`<div class="[^"]*table-wrap[^"]*">\s*(<table\b)`)
	covered := make(map[int]bool)
	for _, match := range wrapped.FindAllStringSubmatchIndex(adminUIHTML, -1) {
		covered[match[2]] = true
	}
	for _, match := range regexp.MustCompile(`<table\b`).FindAllStringIndex(adminUIHTML, -1) {
		if covered[match[0]] {
			continue
		}
		line := strings.Count(adminUIHTML[:match[0]], "\n") + 1
		t.Errorf("index.html:%d 的 <table> 缺少 .table-wrap 横向滚动容器", line)
	}
}

func TestUIShellIconsHaveNoEmoji(t *testing.T) {
	emoji := regexp.MustCompile(`[\x{1F000}-\x{1FAFF}\x{2300}-\x{23FF}\x{2600}-\x{27BF}]`)
	if got := emoji.FindString(adminUIHTML); got != "" {
		t.Errorf("index.html 仍含 emoji %q；图标必须改为内联 SVG", got)
	}
	if got := emoji.FindString(adminUIAllScripts()); got != "" {
		t.Errorf("JS 动态 HTML 仍含 emoji %q；图标必须改为内联 SVG", got)
	}
}
