package admin

import "testing"

func TestUITargetKeySnapshotBehavior(t *testing.T) {
	runAdminNodeTest(t, "ui/targets.test.cjs")
}
