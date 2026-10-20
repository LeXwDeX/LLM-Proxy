package admin

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// This executes actual module code and event registration, beyond source-string checks.
// CI runs the same fixture as a mandatory Node job; local Go-only environments disclose the skip.
func TestUIRuntimeBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("LLMS_REQUIRE_NODE_TESTS") == "1" {
			t.Fatal("Node is required for UI runtime behavior tests")
		}
		t.Skip("Node unavailable: UI runtime behavior unverified; run node scripts/testing/admin_ui_test.cjs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "../../scripts/testing/admin_ui_test.cjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("UI runtime behavior failed: %v\n%s", err, output)
	}
	t.Log(string(output))
}
