package admin

import "testing"

func TestUIIssue70PoolAndQuota(t *testing.T) {
	for _, path := range []string{"ui/pools-issue70.test.cjs", "ui/quota-base-issue70.test.cjs"} {
		t.Run(path, func(t *testing.T) { runAdminNodeTest(t, path) })
	}
}
