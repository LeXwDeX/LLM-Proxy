package admin

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestUITraceEventsPreserveUntrustedIDsAsData(t *testing.T) {
	runAdminNodeTest(t, "ui/trace-events.test.cjs")
}

func TestUIEditableBindingsPreserveRestrictions(t *testing.T) {
	runAdminNodeTest(t, "ui/form-roundtrip.test.cjs")
}

func TestUIClientAccessRendering(t *testing.T) {
	runAdminNodeTest(t, "ui/client-access.test.cjs")
}

func TestUIQuotaAndPricingBehavior(t *testing.T) {
	for _, path := range []string{"ui/pricing-orphan.test.cjs", "ui/js/quota-coefficient.test.cjs"} {
		t.Run(path, func(t *testing.T) { runAdminNodeTest(t, path) })
	}
}

// Issue #54: api() deadlines, caller aborts, 401 redirect and bounded boot retries.
func TestUIStartupTransportResilience(t *testing.T) {
	for _, path := range []string{"ui/js/core.test.cjs", "ui/js/bootstrap.test.cjs"} {
		t.Run(path, func(t *testing.T) { runAdminNodeTest(t, path) })
	}
}

// Issue #54: api() defaults to a 15s deadline. Calls that legitimately run
// longer must say so explicitly; every api() call to these endpoints carries
// the listed timeoutMs. overview.js uses its own request() with a 12s budget.
func TestUILongAdminQueriesDeclareTimeout(t *testing.T) {
	long := map[string]string{
		`"/admin/data/usage/aggregate`:       "timeoutMs: 60000",
		`"/admin/data/trace?limit=" + limit`: "timeoutMs: 60000",
		`"/sync"`:                            "timeoutMs: 60000",
		`"/test-model"`:                      "timeoutMs: 0",
		`"/keys/wakeup"`:                     "timeoutMs: WAKEUP_TIMEOUT_MS",
	}
	call := regexp.MustCompile(`(?s)api\((.*?)\)\s*(?:;|\.then|,\s*$|\n\s*\.then)`)
	seen := map[string]int{}
	for name, source := range adminUIJSSources() {
		if strings.HasSuffix(name, "/overview.js") {
			continue
		}
		for _, match := range call.FindAllStringSubmatch(source, -1) {
			for endpoint, want := range long {
				if !strings.Contains(match[1], endpoint) {
					continue
				}
				seen[endpoint]++
				if !strings.Contains(match[1], want) {
					t.Errorf("%s: api(%s…) 须显式传 %s", name, endpoint, want)
				}
			}
		}
	}
	for endpoint := range long {
		if seen[endpoint] == 0 {
			t.Errorf("未找到 %s 的 api() 调用，检查用例是否过期", endpoint)
		}
	}
	if seen[`"/admin/data/usage/aggregate`] < 3 {
		t.Errorf("usage/aggregate 调用数 %d，应覆盖 clients/quota/usage 三处", seen[`"/admin/data/usage/aggregate`])
	}
}

// The 401 redirect carries next=<current page>; the portal must accept it so
// the user lands back where the session expired.
func TestLoginNextFromExpiredSessionIsAccepted(t *testing.T) {
	for _, next := range []string{"/admin", "/admin?tab=keys", "/admin/?x=1#targets"} {
		if got := sanitizeNext(next); got != next {
			t.Errorf("sanitizeNext(%q) = %q, want unchanged", next, got)
		}
	}
}

func runAdminNodeTest(t *testing.T, path string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("LLMS_REQUIRE_NODE_TESTS") == "1" {
			t.Fatal("Node is required for trace UI behavior tests")
		}
		t.Skip("Node unavailable: trace UI behavior unverified")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("admin UI behavior failed (%s): %v\n%s", path, err, output)
	}
}
