package admin

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// uiBracketCheck 对 JS 源码做结构感知的括号配平检查：跳过字符串、模板串、
// 注释与正则字面量（启发式），任何括号错配或栈残余都视为结构错误并返回带行号的错误。
//
// 背景：PR #42 重写 admin UI 时吞掉了一个函数闭合 `}`，整页内嵌 JS 语法报错不执行，
// 而 go vet / go test / 健康检查均无法发现，问题直达测试实例。此检查器作为
// make test 门禁防止同类回归；其行为已用历史好/坏版本（2b76a4c / 84f4cbe）验证。
func uiBracketCheck(src string) error {
	const (
		modeCode = iota
		modeSQ
		modeDQ
		modeTpl
		modeLineComment
		modeBlockComment
		modeRegex
	)
	type openBrace struct {
		ch   byte
		line int
	}
	var stack []openBrace
	mode := modeCode
	line := 1
	var prev strings.Builder
	appendPrev := func(c byte) {
		s := prev.String()
		if len(s) >= 8 {
			s = s[len(s)-8:]
		}
		prev.Reset()
		prev.WriteString(s)
		prev.WriteByte(c)
	}
	looksLikeRegexStart := func() bool {
		p := prev.String()
		if strings.HasSuffix(p, "return") {
			return true
		}
		if p == "" {
			return false
		}
		last := p[len(p)-1]
		return strings.IndexByte("=(,:[!&|?+;{}%*~-", last) >= 0
	}
	closerFor := func(c byte) byte {
		switch c {
		case ')':
			return '('
		case '}':
			return '{'
		case ']':
			return '['
		}
		return 0
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\n' {
			line++
			if mode == modeLineComment {
				mode = modeCode
			}
			continue
		}
		switch mode {
		case modeSQ:
			if c == '\\' {
				i++
			} else if c == '\'' {
				mode = modeCode
			}
		case modeDQ:
			if c == '\\' {
				i++
			} else if c == '"' {
				mode = modeCode
			}
		case modeTpl:
			if c == '\\' {
				i++
			} else if c == '`' {
				mode = modeCode
			}
		case modeLineComment:
			// 行注释：到换行为止（换行在上方统一处理）
		case modeBlockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				mode = modeCode
				i++
			}
		case modeRegex:
			if c == '\\' {
				i++
			} else if c == '/' {
				mode = modeCode
			}
		default: // modeCode
			if c == '/' && i+1 < len(src) && src[i+1] == '/' {
				mode = modeLineComment
				i++
			} else if c == '/' && i+1 < len(src) && src[i+1] == '*' {
				mode = modeBlockComment
				i++
			} else if c == '\'' || c == '"' || c == '`' {
				switch c {
				case '\'':
					mode = modeSQ
				case '"':
					mode = modeDQ
				case '`':
					mode = modeTpl
				}
			} else if c == '/' {
				if looksLikeRegexStart() {
					mode = modeRegex
				}
			} else if c == '(' || c == '{' || c == '[' {
				stack = append(stack, openBrace{ch: c, line: line})
			} else if c == ')' || c == '}' || c == ']' {
				want := closerFor(c)
				if len(stack) == 0 || stack[len(stack)-1].ch != want {
					got := "栈空"
					if len(stack) > 0 {
						got = string(stack[len(stack)-1].ch)
					}
					return fmt.Errorf("第 %d 行: 多余或不匹配的 %c（栈顶期望闭合 %c，实际栈顶=%s）", line, c, want, got)
				}
				stack = stack[:len(stack)-1]
			}
			if c != ' ' && c != '\t' && c != '\r' {
				appendPrev(c)
			}
		}
	}
	if len(stack) > 0 {
		top := stack[len(stack)-1]
		return fmt.Errorf("源码结束仍有未闭合的 %c（第 %d 行打开），共 %d 个未闭合括号", top.ch, top.line, len(stack))
	}
	return nil
}

var inlineScriptRe = regexp.MustCompile(`(?s)<script\b[^>]*>(.*?)</script>`)

// extractInlineScripts 提取 HTML 中所有不含 src 属性的内联 <script> 块。
func extractInlineScripts(html string) []string {
	var out []string
	for _, m := range inlineScriptRe.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestUIInlineScriptsBalanced 校验管理台内联和全部嵌入 JS 模块的括号配平。
// 内嵌 JS 存在语法错误时整页脚本不执行（页面卡"加载中"、菜单不渲染），
// 且 go vet / 健康检查均无法发现，必须在此拦截。
func TestUIInlineScriptsBalanced(t *testing.T) {
	for i, script := range extractInlineScripts(adminUIHTML) {
		if err := uiBracketCheck(script); err != nil {
			t.Errorf("index.html 内联 script #%d 括号配平失败: %v", i+1, err)
		}
	}
	for name, src := range adminUIJSSources() {
		if err := uiBracketCheck(src); err != nil {
			t.Errorf("%s 括号配平失败: %v", name, err)
		}
	}
}

// TestUIBracketChecker 用最小样本校验检查器本身：好样本必须通过，
// 坏样本必须失败——含 PR #42 引入的「插入函数吞掉外层闭合 }」的等价最小复现。
func TestUIBracketChecker(t *testing.T) {
	good := []string{
		"",
		"(function(){ var s = \"){\"; /* ( 未配对的注释 */ if (a) { b(); } })();",
		"var re = /=/; var tpl = `x${1+1}y ) (`; var q = 'a(b'; var w = \"c)d\";",
		"items.forEach(function(x) { use(x / 2); });",
	}
	for i, src := range good {
		if err := uiBracketCheck(src); err != nil {
			t.Errorf("好样本 #%d 不应报错，却得到: %v", i+1, err)
		}
	}
	bad := []struct {
		src  string
		note string
	}{
		{"(function(){ function f() { return 1; function g() { return 2; } })();", "插入函数吞掉外层闭合 }（PR #42 的等价最小复现）"},
		{"function a() { function b() { return 1; } ", "结尾存在未闭合括号"},
		{"var x = (1 + 2));", "多余的右括号"},
		{"var x = {a: 1];", "闭合符与栈顶不匹配"},
	}
	for i, tc := range bad {
		err := uiBracketCheck(tc.src)
		if err == nil {
			t.Errorf("坏样本 #%d（%s）应报错却通过了", i+1, tc.note)
			continue
		}
		if !strings.Contains(err.Error(), "行") {
			t.Errorf("坏样本 #%d 的错误信息应包含行号，实际: %v", i+1, err)
		}
	}
}

var (
	scriptBlockRe = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
	styleBlockRe  = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	divOpenRe     = regexp.MustCompile(`(?i)<div\b`)
	divCloseRe    = regexp.MustCompile(`(?i)</div>`)
	overlayOpenRe = regexp.MustCompile(`(?i)<div[^>]+class="[^"]*(?:modal-overlay|quota-drawer-overlay)[^"]*"`)
	tagIdRe       = regexp.MustCompile(`(?i)id="([^"]+)"`)

	// #64：弹层提取改按完整 class token 识别，支持多 class 与属性乱序（见 extractOverlayIDs）。
	tagOpenRe      = regexp.MustCompile(`(?is)<[a-z][^>]*>`)
	classAttrRe    = regexp.MustCompile(`(?i)\bclass=["']([^"']*)["']`)
	tagIdAttrRe    = regexp.MustCompile(`(?i)\bid=["']([^"']+)["']`)
	overlayTokenRe = regexp.MustCompile(`(?:^|\s)(?:modal-overlay|quota-drawer-overlay)(?:\s|$)`)
	idMarkRe       = regexp.MustCompile(`id:([A-Za-z0-9_-]+)`)
)

// extractOverlayIDs 提取 class 含 modal-overlay / quota-drawer-overlay（完整 token，
// 顺序不限、允许多 class）的元素 id，按字典序去重返回；无 id 属性的元素跳过。
// 背景（#64）：旧正则只认「class 值以弹层类结尾且紧邻 id 属性」的形态，弹层一旦
// 追加第二个 class 或调整属性顺序，就会静默绕过清单登记门禁（漏检直达人眼验收）。
func extractOverlayIDs(html string) []string {
	seen := map[string]bool{}
	for _, tag := range tagOpenRe.FindAllString(html, -1) {
		cm := classAttrRe.FindStringSubmatch(tag)
		if cm == nil || !overlayTokenRe.MatchString(cm[1]) {
			continue
		}
		if im := tagIdAttrRe.FindStringSubmatch(tag); im != nil {
			seen[im[1]] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// unregisteredOverlays 返回 html 中提取到、但未以 id:xxx 登记于 checklist 的弹层 id。
// 供 TestUIChecklistSync 与对抗测试共用同一套门禁判定逻辑，避免测试自证。
func unregisteredOverlays(html, checklist string) []string {
	registered := map[string]bool{}
	for _, m := range idMarkRe.FindAllStringSubmatch(checklist, -1) {
		registered[m[1]] = true
	}
	var missing []string
	for _, id := range extractOverlayIDs(html) {
		if !registered[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

// TestUIOverlaysMountedAtBody 校验所有弹层（.modal-overlay / .quota-drawer-overlay）
// 挂载于 body 直连（弹层开标签时 div 深度 == 0），并顺带断言全文 div 标签配平。
//
// 背景：PR #42 时 clientModal 内 .modal-body 缺一个闭合 </div>，clientModal 吞掉
// 后续 9 个弹层；clientModal 未打开时 display:none 连坐全部后代——编辑额度/新增职级/
// 新增项目组/确认框等全部不可见，且无任何报错，直到人工验收才暴露。
// 统一规范：弹层一律 body 直连、仅经 openModal/closeModal 开关（见 internal/admin/AGENTS.md）。
// traceDetailModal 豁免：页内详情层（display:none 内联控制），为两侧历史版本一致的有意设计。
func TestUIOverlaysMountedAtBody(t *testing.T) {
	cleaned := htmlCommentRe.ReplaceAllString(adminUIHTML, "")
	cleaned = scriptBlockRe.ReplaceAllString(cleaned, "")
	cleaned = styleBlockRe.ReplaceAllString(cleaned, "")

	lines := strings.Split(cleaned, "\n")
	depth := 0
	for i, line := range lines {
		if m := overlayOpenRe.FindString(line); m != "" {
			id := ""
			if idm := tagIdRe.FindStringSubmatch(line); idm != nil {
				id = idm[1]
			}
			// traceDetailModal 豁免：页内详情层（display:none 内联控制），两侧历史版本一致的有意设计。
			if id != "traceDetailModal" && depth != 0 {
				t.Errorf("第 %d 行弹层（id=%s）挂载深度=%d，必须为 body 直连（深度 0）——嵌套会在宿主隐藏时连坐不可见", i+1, id, depth)
			}
		}
		depth += len(divOpenRe.FindAllString(line, -1)) - len(divCloseRe.FindAllString(line, -1))
	}
	if depth != 0 {
		t.Errorf("HTML div 标签不配平：净深度 %d（应为 0）——缺失或多余的 </div> 会吞掉后续弹层", depth)
	}
}

// stripJsStrings 结构感知地剥离字符串/模板串/注释/正则字面量，仅保留代码骨架
// （状态机与 uiBracketCheck 一致），供未定义调用分析使用。
func stripJsStrings(src string) string {
	const (
		modeCode = iota
		modeSQ
		modeDQ
		modeTpl
		modeLineComment
		modeBlockComment
		modeRegex
	)
	var out strings.Builder
	mode := modeCode
	var prev strings.Builder
	appendPrev := func(c byte) {
		s := prev.String()
		if len(s) >= 8 {
			s = s[len(s)-8:]
		}
		prev.Reset()
		prev.WriteString(s)
		prev.WriteByte(c)
	}
	looksLikeRegexStart := func() bool {
		p := prev.String()
		if strings.HasSuffix(p, "return") {
			return true
		}
		if p == "" {
			return false
		}
		return strings.IndexByte("=(,:[!&|?+;{}%*~-", p[len(p)-1]) >= 0
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\n' {
			if mode == modeLineComment {
				mode = modeCode
			}
			if mode == modeCode {
				out.WriteByte(c)
			}
			continue
		}
		switch mode {
		case modeSQ:
			if c == '\\' {
				i++
			} else if c == '\'' {
				mode = modeCode
			}
		case modeDQ:
			if c == '\\' {
				i++
			} else if c == '"' {
				mode = modeCode
			}
		case modeTpl:
			if c == '\\' {
				i++
			} else if c == '`' {
				mode = modeCode
			}
		case modeLineComment:
		case modeBlockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				mode = modeCode
				i++
			}
		case modeRegex:
			if c == '\\' {
				i++
			} else if c == '/' {
				mode = modeCode
			}
		default:
			if c == '/' && i+1 < len(src) && src[i+1] == '/' {
				mode = modeLineComment
				i++
			} else if c == '/' && i+1 < len(src) && src[i+1] == '*' {
				mode = modeBlockComment
				i++
			} else if c == '\'' || c == '"' || c == '`' {
				switch c {
				case '\'':
					mode = modeSQ
				case '"':
					mode = modeDQ
				case '`':
					mode = modeTpl
				}
			} else if c == '/' {
				if looksLikeRegexStart() {
					mode = modeRegex
				} else {
					out.WriteByte(c)
				}
			} else {
				out.WriteByte(c)
			}
			if c != ' ' && c != '\t' && c != '\r' {
				appendPrev(c)
			}
		}
	}
	return out.String()
}

// TestUIUndefinedCalls 扫描内联脚本与全部嵌入模块的「未声明标识符」函数调用。
// 背景：PR #42/#43 删除 ensureCatalogAll/fillCatalogDatalist/loadGroups 等函数时
// 残留调用点，导致新增目标/编辑目标弹窗与分组页静默抛 ReferenceError（弹窗不打开、
// 页面空白），直至人工验收才暴露。此门禁保证删除函数必须同步清理调用点。
// 普通函数和匿名回调的简单参数绑定纳入声明收集；字符串/注释不能产生声明。
func uiJSDeclaredNames(src string) map[string]bool {
	declared := map[string]bool{}
	code := stripJsStrings(src)
	fnDeclRe := regexp.MustCompile(`function\s+([A-Za-z_$][\w$]*)\s*\(`)
	varDeclRe := regexp.MustCompile(`(?:^|[;\s{,(])(?:var|let|const)\s+([A-Za-z_$][\w$]*)`)
	for _, re := range []*regexp.Regexp{fnDeclRe, varDeclRe} {
		for _, match := range re.FindAllStringSubmatch(code, -1) {
			declared[match[1]] = true
		}
	}
	paramRe := regexp.MustCompile(`\bfunction(?:\s+[A-Za-z_$][\w$]*)?\s*\(([^)]*)\)`)
	identifierRe := regexp.MustCompile(`^[A-Za-z_$][\w$]*$`)
	for _, match := range paramRe.FindAllStringSubmatch(code, -1) {
		for _, binding := range strings.Split(match[1], ",") {
			if name := strings.TrimSpace(binding); identifierRe.MatchString(name) {
				declared[name] = true
			}
		}
	}
	return declared
}

func uiJSUndefinedCalls(src string, declared, builtin map[string]bool) []string {
	var names []string
	callRe := regexp.MustCompile(`(?:^|[^\w$.])([A-Za-z_$][\w$]*)\s*\(`)
	for _, match := range callRe.FindAllStringSubmatch(stripJsStrings(src), -1) {
		if name := match[1]; !declared[name] && !builtin[name] {
			names = append(names, name)
		}
	}
	return names
}

func TestUIUndefinedCallsRecognizesFunctionParameters(t *testing.T) {
	src := `function executor(resolve, reject) {
		reject(new Error("offline"));
		rejcet();
		Promise.reject(new Error("method"));
	}
	var callback = function (done) { done(); };
	var literal = "function (fake) { fake(); }";`
	declared := uiJSDeclaredNames(src)
	builtin := map[string]bool{"function": true, "Error": true}
	if got := uiJSUndefinedCalls(src, declared, builtin); !reflect.DeepEqual(got, []string{"rejcet"}) {
		t.Fatalf("undefined calls = %v, want only the actual typo rejcet", got)
	}
	if declared["fake"] {
		t.Fatal("string contents must not declare callable parameters")
	}
}

func TestUIUndefinedCalls(t *testing.T) {
	declared := map[string]bool{}
	builtin := map[string]bool{
		"function": true, "if": true, "for": true, "while": true, "switch": true, "catch": true,
		"return": true, "typeof": true, "new": true, "delete": true, "void": true, "in": true,
		"of": true, "instanceof": true, "do": true, "else": true, "try": true, "finally": true,
		"throw": true, "break": true, "continue": true, "case": true, "default": true,
		"Promise": true, "Number": true, "String": true, "Boolean": true, "Array": true,
		"Object": true, "Date": true, "Math": true, "JSON": true, "RegExp": true, "Map": true,
		"Set": true, "Error": true, "TypeError": true, "encodeURIComponent": true,
		"decodeURIComponent": true, "parseInt": true, "parseFloat": true, "isNaN": true,
		"setTimeout": true, "setInterval": true, "clearTimeout": true, "clearInterval": true,
		"requestAnimationFrame": true, "fetch": true, "alert": true, "confirm": true,
		"prompt": true, "document": true, "window": true, "console": true, "navigator": true,
		"location": true, "history": true, "localStorage": true, "sessionStorage": true,
		"FormData": true, "Blob": true, "File": true, "FileReader": true, "URL": true,
		"URLSearchParams": true, "AbortController": true, "ResizeObserver": true, "CustomEvent": true, "Event": true,
		"crypto": true, "structuredClone": true, "queueMicrotask": true, "Symbol": true,
		"Proxy": true, "Reflect": true, "BigInt": true, "isFinite": true,
	}
	bridgeRe := regexp.MustCompile(`window\.(\w+)\s*=\s*\w+\s*;`)

	var inline strings.Builder
	for _, s := range extractInlineScripts(adminUIHTML) {
		inline.WriteString(s)
		inline.WriteString("\n;\n")
	}
	sources := adminUIJSSources()
	sources["index.html 内联脚本"] = inline.String()
	for _, src := range sources {
		for name := range uiJSDeclaredNames(src) {
			declared[name] = true
		}
	}
	for _, m := range bridgeRe.FindAllStringSubmatch(adminUIAllScripts(), -1) {
		declared[m[1]] = true
	}

	for name, src := range sources {
		for _, undefined := range uiJSUndefinedCalls(src, declared, builtin) {
			t.Errorf("%s 调用了未声明的函数 %q——若为删除函数的残留调用请同步清理；若为误报请完善声明收集", name, undefined)
		}
	}
}

// TestUIMissingElementIds 校验 JS 中 getElementById 引用的 id 在页面中存在
// （静态 id 或 JS 模板动态生成的 id 均计入声明集）。
// 背景：#43 删除「分组」输入框时残留 saveTarget 中 getElementById("tgtGroup").value
// 的读取，保存目标必抛 TypeError——无请求、无 toast，直至验收才暴露。
func TestUIMissingElementIds(t *testing.T) {
	idDeclRe := regexp.MustCompile(`\bid=["']([^"']+)["']`)
	declared := map[string]bool{}
	for _, m := range idDeclRe.FindAllStringSubmatch(adminUIHTML, -1) {
		declared[m[1]] = true
	}
	for _, m := range idDeclRe.FindAllStringSubmatch(adminUIAllScripts(), -1) {
		declared[m[1]] = true
	}
	refRe := regexp.MustCompile(`getElementById\(\s*["']([^"']+)["']\s*\)`)
	for name, src := range adminUIJSSources() {
		for _, m := range refRe.FindAllStringSubmatch(src, -1) {
			if !declared[m[1]] {
				t.Errorf("%s 引用了不存在的元素 id %q——删除元素时请同步清理引用", name, m[1])
			}
		}
	}
}

// TestUINewButtonsBound 校验所有 id 形如 new…Btn 的新增类主操作按钮在 JS 中
// 至少被 getElementById 引用一次（即有绑定机会）。
// 背景：#50 计费模板「新增模板」按钮只有 HTML 没有 click 绑定，点击无反应、
// 无任何报错；TestUIMissingElementIds 只能发现「引用不存在的 id」，
// 无法发现「元素存在但无人绑定」的反向缺失。
func TestUINewButtonsBound(t *testing.T) {
	btnDeclRe := regexp.MustCompile(`<button[^>]*\bid=["'](new[A-Za-z]*Btn)["']`)
	declared := map[string]bool{}
	for _, m := range btnDeclRe.FindAllStringSubmatch(adminUIHTML, -1) {
		declared[m[1]] = true
	}
	refRe := regexp.MustCompile(`getElementById\(\s*["']([^"']+)["']\s*\)`)
	referenced := map[string]bool{}
	for _, src := range []string{adminUIAllScripts()} {
		for _, m := range refRe.FindAllStringSubmatch(src, -1) {
			referenced[m[1]] = true
		}
	}
	for id := range declared {
		if !referenced[id] {
			t.Errorf("新增类按钮 %q 在 JS 中无任何 getElementById 引用——请补 click 绑定或移除死按钮", id)
		}
	}
}

// TestUIAdminOnlyMenuGuard 校验 admin-only 菜单（用户管理）默认隐藏、由 JS 按
// admin 角色显示。背景：/admin/data/users 系列接口对 readonly 会话返回 403，
// 若菜单入口对 readonly 可见，点击只会得到一张永远「加载失败」的页面——
// 入口可见性是第一道防线，后端 403 是第二道（纵深防御，两者都必须保留）。
func TestUIAdminOnlyMenuGuard(t *testing.T) {
	navRe := regexp.MustCompile(`(?is)<div[^>]*id="nav-users"[^>]*>`)
	nav := navRe.FindString(adminUIHTML)
	if nav == "" {
		t.Fatalf("侧边栏缺少 id=nav-users 的菜单项（用户管理入口）")
	}
	if !strings.Contains(nav, `data-page="users"`) {
		t.Errorf("nav-users 必须携带 data-page=\"users\" 才能接入页面切换")
	}
	if !strings.Contains(nav, "display:none") && !strings.Contains(nav, "display: none") {
		t.Errorf("nav-users 必须默认隐藏（内联 display:none），禁止对 readonly 登录暴露 admin-only 入口")
	}
	if !strings.Contains(adminUIAllScripts(), `getElementById("nav-users")`) {
		t.Errorf("JS 中缺少对 nav-users 的 getElementById 引用——admin 角色显示逻辑缺失，菜单将永远不可见")
	}
	if !strings.Contains(adminUIAllScripts(), `data.role === "admin"`) {
		t.Errorf("JS 中缺少 data.role === \"admin\" 分支——admin-only 菜单的显示依据丢失")
	}
}

// TestUIUserRowActionsGuard 校验用户管理页的动态行内操作（编辑/禁用/删除）具备
// 完整闭环：事件委托绑定、双模式弹窗标题、默认账号保护。背景：行内按钮由
// loadUsers 动态渲染，TestUINewButtonsBound 只覆盖静态 new…Btn 按钮——动态按钮
// 若漏绑委托（PR #50 同类事故），点击无反应且无任何报错，直达验收才暴露。
func TestUIUserRowActionsGuard(t *testing.T) {
	for _, sel := range []string{"data-edit-user", "data-del-user", "data-toggle-user"} {
		if !strings.Contains(adminUIAllScripts(), `closest("[`+sel+`]")`) {
			t.Errorf("用户列表缺少 %q 行内按钮的事件委托绑定——动态按钮将点击无反应", sel)
		}
	}
	if !strings.Contains(adminUIHTML, `id="userModalTitle"`) {
		t.Errorf("userModal 缺少 id=userModalTitle（新增/编辑双模式标题元素）")
	}
	if !strings.Contains(adminUIHTML, `id="userPasswordHint"`) {
		t.Errorf("userModal 缺少 id=userPasswordHint（编辑模式「留空则不修改」提示元素）")
	}
	if !strings.Contains(adminUIAllScripts(), "DEFAULT_ADMIN_USERNAME") {
		t.Errorf("JS 缺少 DEFAULT_ADMIN_USERNAME 守卫——默认 admin 账号行将渲染编辑/删除按钮，绕过后端保护预期")
	}
}

// TestUIChecklistSync 强制当前回归清单覆盖 admin UI 全部页面与弹层。
// 新增页面/弹窗若漏登清单，人眼验收永远验不到它。清单以 `id:xxx` 标记登记项，
// 本门禁对 data-page 与 modal/drawer overlay id 求差集，漏登即红。
func TestUIChecklistSync(t *testing.T) {
	checklist, err := os.ReadFile("testdata/回归测试清单.md")
	if err != nil {
		t.Fatalf("读取回归清单失败（新增页面/弹窗需同步登记）: %v", err)
	}
	registered := map[string]bool{}
	for _, m := range idMarkRe.FindAllStringSubmatch(string(checklist), -1) {
		registered[m[1]] = true
	}
	pages := map[string]bool{}
	pageRe := regexp.MustCompile(`data-page="([a-z-]+)"`)
	for _, m := range pageRe.FindAllStringSubmatch(adminUIHTML, -1) {
		pages[m[1]] = true
	}
	for page := range pages {
		if !registered[page] {
			t.Errorf("页面 %q 未登记在 internal/admin/testdata/回归测试清单.md——请补人眼验收项（id:%s）", page, page)
		}
	}
	// #64：提取走 extractOverlayIDs（完整 class token、多 class、属性乱序均可识别）。
	for _, overlay := range unregisteredOverlays(adminUIHTML, string(checklist)) {
		t.Errorf("弹层 %q 未登记在 internal/admin/testdata/回归测试清单.md——请补人眼验收项（id:%s）", overlay, overlay)
	}
}

// TestUIChecklistSyncMultiClassOverlays 是 #64 的对抗测试：多 class 弹层既要被提取
// （敏感度不回退），未登记时也必须使门禁失败——防止「只放宽正则、不证明漏检会被抓住」。
func TestUIChecklistSyncMultiClassOverlays(t *testing.T) {
	html := `
	<div class="modal-overlay wide" id="wideModal"></div>
	<div id="frontId" class="drawer-mask quota-drawer-overlay"></div>
	<div class='quoted modal-overlay' id="quotedModal"></div>
	<div class="not-modal-overlay-lookalike" id="decoyModal"></div>
	`
	// 正向：多 class / 属性乱序 / 单引号弹层必须全部提取；非弹层 token 不得误报。
	got := extractOverlayIDs(html)
	want := []string{"frontId", "quotedModal", "wideModal"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("多 class 弹层提取结果不符：got=%v want=%v", got, want)
	}

	// 对抗：同一批多 class 弹层未登记清单时，真实门禁逻辑必须判定全部缺失。
	checklist := "id:knownPage\nid:someRegisteredModal"
	missing := unregisteredOverlays(html, checklist)
	if !reflect.DeepEqual(missing, want) {
		t.Errorf("未登记的多 class 弹层应全部被门禁判为缺失，实际缺失=%v", missing)
	}

	// 对照：全部登记后门禁必须通过（排除「永远红」的假阳性实现）。
	full := checklist + "\nid:wideModal\nid:frontId\nid:quotedModal"
	if extra := unregisteredOverlays(html, full); len(extra) != 0 {
		t.Errorf("已登记的多 class 弹层不应报缺失，实际=%v", extra)
	}
}
