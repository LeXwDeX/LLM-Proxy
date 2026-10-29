package admin

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// adminUIJSSources enumerates the embedded scripts for contract checks.
func adminUIJSSources() map[string]string {
	sources := map[string]string{}
	err := fs.WalkDir(adminUIJSFS, "ui/js", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(name, ".js") {
			return err
		}
		data, err := adminUIJSFS.ReadFile(name)
		if err != nil {
			return err
		}
		sources[name] = string(data)
		return nil
	})
	if err != nil {
		panic(err) // The tree is guaranteed by go:embed at build time.
	}
	return sources
}

// adminUIAllScripts lets source-level checks follow moved code. Browser
// execution order is defined by the script tags in index.html.
func adminUIAllScripts() string {
	sources := adminUIJSSources()
	keys := make([]string, 0, len(sources))
	for key := range sources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var result strings.Builder
	result.WriteString(adminUIHTML)
	for _, key := range keys {
		result.WriteString("\n;\n")
		result.WriteString(sources[key])
	}
	return result.String()
}

// TestUIBillingBasisNoticesVisible 要求用量页、总览历史费用区、模型计费页、客户端页与额度页常驻显示计费口径，
// 且说明块不位于会被 JS 整体重绘的容器内，不使用悬停 Tooltip 或隐藏属性。
func TestUIBillingBasisNoticesVisible(t *testing.T) {
	fullKeywords := []string{"官方", "不估算", "连接断开，未获取到计费字段", "少于厂商官方账单", "目录默认价"}
	shortKeywords := []string{"官方", "目录默认价", "不估算 token"}
	cases := []struct {
		page, id string
		keywords []string
	}{
		{"page-usage", "usageBillingBasis", fullKeywords},
		{"page-overview", "overviewBillingBasis", fullKeywords},
		{"page-model-costs", "pricingBillingBasis", shortKeywords},
		{"page-clients", "clientsBillingBasis", shortKeywords},
		{"page-quota", "quotaBillingBasis", shortKeywords},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			page := uiPageSection(t, tc.page)
			marker := `id="` + tc.id + `"`
			start := strings.Index(page, marker)
			if start < 0 {
				t.Fatalf("%s 缺少常驻计费口径说明 %s", tc.page, marker)
			}
			tagStart := strings.LastIndex(page[:start], "<")
			tagEnd := start + strings.Index(page[start:], ">")
			openTag := page[tagStart : tagEnd+1]
			for _, forbidden := range []string{"hidden", `role="tooltip"`, "data-activity-info", "display:none"} {
				if strings.Contains(openTag, forbidden) {
					t.Errorf("%s 不得使用 %q：说明须常驻可见，不能依赖悬停或隐藏", tc.id, forbidden)
				}
			}
			body := page[tagEnd+1:]
			closeIdx := strings.Index(body, "</"+tagName(openTag)+">")
			if closeIdx < 0 {
				t.Fatalf("%s 说明块未闭合", tc.id)
			}
			text := strings.NewReplacer("<strong>", "", "</strong>", "").Replace(body[:closeIdx])
			for _, keyword := range tc.keywords {
				if !strings.Contains(text, keyword) {
					t.Errorf("%s 说明缺少关键词 %q；实际：%s", tc.id, keyword, text)
				}
			}
		})
	}
	// 总览的费用表容器会被 overview.js 以 innerHTML 重绘，说明必须是其后的兄弟节点。
	overview := uiPageSection(t, "page-overview")
	summary := `<div class="activity-table-wrap" id="overviewUsageSummary">正在加载历史费用…</div>`
	si, ni := strings.Index(overview, summary), strings.Index(overview, `id="overviewBillingBasis"`)
	if si < 0 || ni < si+len(summary) {
		t.Errorf("overviewBillingBasis 必须位于 #overviewUsageSummary 之外且紧随其后，避免被费用表重绘覆盖")
	}
	for _, id := range []string{"overviewBillingBasis", "usageBillingBasis", "pricingBillingBasis", "clientsBillingBasis", "quotaBillingBasis"} {
		if strings.Contains(adminUIAllScripts()[len(adminUIHTML):], id) {
			t.Errorf("%s 是静态说明，JS 不应改写", id)
		}
	}
}

func tagName(openTag string) string {
	name := strings.TrimPrefix(openTag, "<")
	if i := strings.IndexAny(name, " >"); i >= 0 {
		name = name[:i]
	}
	return name
}

// uiPageSection 返回 index.html 中指定 .page 容器起点到下一个页面起点之间的源码。
func uiPageSection(t *testing.T, pageID string) string {
	t.Helper()
	marker := `id="` + pageID + `"`
	start := strings.Index(adminUIHTML, marker)
	if start < 0 {
		t.Fatalf("index.html 缺少页面 %s", pageID)
	}
	rest := adminUIHTML[start+len(marker):]
	if end := strings.Index(rest, `<div class="page`); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// TestUIOverviewRangeLayout 锁定总览的区间选择与分区结构：五个本地区间（不含旧的近 1 小时），
// 区间卡片、两张区间趋势图、两张排行表与实时卡片均存在；区间数据只请求小时粒度聚合。
func TestUIOverviewRangeLayout(t *testing.T) {
	overview := uiPageSection(t, "page-overview")
	var ranges []string
	for rest := overview; ; {
		i := strings.Index(rest, `data-overview-range="`)
		if i < 0 {
			break
		}
		rest = rest[i+len(`data-overview-range="`):]
		ranges = append(ranges, rest[:strings.Index(rest, `"`)])
	}
	if got, want := strings.Join(ranges, ","), "today,yesterday,7d,30d,last_month"; got != want {
		t.Errorf("总览区间按钮 = %s，want %s", got, want)
	}
	for _, id := range []string{"ovRangeWindow", "ovRangeRequests", "ovRangeTotalTokens", "ovRangeCost", "ovRangeClients", "ovRangeModels",
		"chainSummary", "chainStatus", "chainOpsNotice", "activityActiveRequests", "chainTargets", "chainKeys", "ovLiveSuccessRate",
		"ovTokenTrend", "ovBarTrend", "activityConcurrencyChart", "activityTokenChart", "topClientsBody", "topModelsBody",
		"targetHealthBody", "activityClientsBody", "recentChangesBody"} {
		if !strings.Contains(overview, `id="`+id+`"`) {
			t.Errorf("总览缺少 #%s", id)
		}
	}
	if strings.Contains(overview, "overviewAttention") {
		t.Error("总览已移除「需要关注」面板，不应再渲染 #overviewAttention")
	}
	if strings.Contains(overview, "overview-chain-node") {
		t.Error("链路摘要已并入实时状态卡片，不应再单独渲染")
	}
	js := adminUIPageScripts(t, "overview")
	if !strings.Contains(js, "group_by=hour") || strings.Contains(js, "group_by=day") {
		t.Error("总览区间数据必须只请求 group_by=hour 聚合，由前端汇总本地自然日")
	}
}

var uiScriptTagRe = regexp.MustCompile(`<script\s+src="/admin/ui/js/([^"?]+\.js)(?:\?[^"]*)?"[^>]*></script>`)

// adminUIScriptManifest returns the script paths (relative to ui/js) in the
// order the assembled HTML loads them.
func adminUIScriptManifest() []string {
	var out []string
	for _, m := range uiScriptTagRe.FindAllStringSubmatch(adminUIHTML, -1) {
		out = append(out, m[1])
	}
	return out
}

// adminUIScript returns one embedded script ("core/core.js"); it fails when
// the file is missing or empty so a moved file cannot make a check vacuous.
func adminUIScript(t testing.TB, rel string) string {
	t.Helper()
	src := adminUIJSSources()["ui/js/"+rel]
	if strings.TrimSpace(src) == "" {
		t.Fatalf("script ui/js/%s is missing or empty", rel)
	}
	return src
}

// adminUIPageScripts concatenates every embedded .js file in ui/js/pages/<page>/
// (sorted by path), so checks keep working when a page is split into several
// files. It fails when the directory has no scripts or they are all empty.
func adminUIPageScripts(t testing.TB, page string) string {
	t.Helper()
	prefix := "ui/js/pages/" + page + "/"
	var keys []string
	sources := adminUIJSSources()
	for key := range sources {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		t.Fatalf("no scripts under %s", prefix)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString(sources[key])
		b.WriteString("\n;\n")
	}
	if strings.TrimSpace(strings.ReplaceAll(b.String(), ";", "")) == "" {
		t.Fatalf("scripts under %s are empty", prefix)
	}
	return b.String()
}
