package admin

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestIssue69LoginNextBoundaries(t *testing.T) {
	valid := []string{
		"/admin", "/admin/", "/admin?tab=keys&view=all", "/admin/ui?tab=keys#targets",
		"/admin/data/clients?name=a%2Fb&next=%2Foutside",
		"/admin/data/../ui?tab=keys&view=all", "/admin/data/%2e%2e/ui?tab=keys",
	}
	for _, next := range valid {
		if got := sanitizeNext(next); got != next {
			t.Errorf("sanitizeNext(%q) = %q, want unchanged", next, got)
		}
	}
	invalid := []string{
		"", "/administrator", "/administer?tab=x", "/admin/../outside", "/admin/%2e%2e/outside",
		"/admin%2f..%2foutside", "/admin/../../outside", "/admin\\..\\outside", "/admin/%5c..%5coutside",
		"https://example.invalid/admin", "//example.invalid/admin", "/admin/%zz", "/admin/\noutside",
	}
	for _, next := range invalid {
		if got := sanitizeNext(next); got != "/admin" {
			t.Errorf("sanitizeNext(%q) = %q, want /admin", next, got)
		}
	}
}

func TestIssue69LoginNextPostPreservesQueryAndFallsBack(t *testing.T) {
	portal, users := newPortalTestFixture(t)
	if err := users.Create(config.AdminUser{Username: "alice", Role: RoleAdmin, PasswordHash: HashPassword("password1", "test-salt")}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ next, want string }{
		{"/admin/ui?tab=keys&view=all", "/admin/ui?tab=keys&view=all"},
		{"/admin/data/../ui?tab=keys&view=all", "/admin/ui?tab=keys&view=all"},
		{"/admin/../outside?tab=keys", "/admin"},
		{"/administrator?tab=keys", "/admin"},
	} {
		w := postLogin(t, portal, url.Values{"username": {"alice"}, "password": {"password1"}, "next": {tc.next}})
		if w.Code != http.StatusFound || w.Header().Get("Location") != tc.want {
			t.Errorf("login next %q: status %d, location %q; want redirect to %q", tc.next, w.Code, w.Header().Get("Location"), tc.want)
		}
	}
}

func TestUIIssue69SessionRole(t *testing.T) {
	runAdminNodeTest(t, "ui/session-role-issue69.test.cjs")
}
