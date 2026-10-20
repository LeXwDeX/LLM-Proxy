package admin

import (
	"context"
	"os"
	"os/exec"
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
