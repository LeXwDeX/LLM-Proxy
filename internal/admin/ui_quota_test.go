package admin

import (
	"regexp"
	"strings"
	"testing"
)

func TestHandlerUIQuotaManagementFeatures(t *testing.T) {
	required := []string{
		`body.readonly-mode .readonly-hide`,
		`body.readonly-mode .readonly-disable`,
		`class="panel readonly-hide"`,
		`id="quotaSyncSource"`,
		`id="quotaSyncProjectFilter"`,
		`id="quotaSyncTargetsBtn"`,
		`id="quotaSyncTargetsPanel"`,
		`#quotaSyncTargetsPanel input:checked`,
		`function quotaConvertedDetail(status)`,
		`职级可用额度倍率`,
		`class='quota-detail-table quota-group-table'`,
		`模型组折算消耗与可用额度上限`,
		`未绑定职级：未配置额度`,
		`可用额度上限预览`,
		`data-model-binding-target`,
		`target.model_mappings`,
		`function applyReadonlyMode(root)`,
		`control.disabled = true;`,
		`if (isReadonlyMode()) return;`,
		`await app.modules.users.loadUserInfo();`,
		`id="newJobLevelBtn"`,
		`id="jobLevelsBody"`,
		`id="newProjectGroupBtn"`,
		`id="projectGroupsBody"`,
		`id="dictModal"`,
		`/admin/data/job-levels`,
		`/admin/data/project-groups`,
		// 页签结构与人员额度面板（2026-08 重组）
		`data-quota-tab="people"`,
		`data-quota-tab="policy"`,
		`data-quota-tab="batch"`,
		`data-quota-tab="settings"`,
		`id="quota-tab-people"`,
		`id="quotaSearch"`,
		`id="quotaSummaryProjectFilter"`,
		`id="quotaSummaryJobFilter"`,
		`id="quotaSummaryStatusFilter"`,
		`data-expand-quota`,
		`data-edit-quota`,
		`.quota-bar-inner.q-over`,
		`.quota-bar-inner.q-warn`,
		`.quota-badge.over`,
		// 编辑额度抽屉：职级绑定 + plan 个人系数（额度体系 v2）
		`id="quotaEditDrawer"`,
		`id="quotaEditJobLevel"`,
		`id="saveQuotaEditBtn"`,
		`已选模型组`,
		`data-edit-coefficient`,
		`quota-edit-limit-row`,
		// 职级结构化弹窗（三系数）
		`id="jobLevelModal"`,
		`id="saveJobLevelBtn"`,
		`dim.job_coefficient_field`,
		// 批量同步：范围模式 + 二次确认与说明
		`id="quotaSyncMode"`,
		`id="quotaSyncProjectTarget"`,
		`id="quotaSyncJobTarget"`,
		`id="quotaSyncHint"`,
	}
	for _, marker := range required {
		if !strings.Contains(adminUIHTML+adminUIAllScripts(), marker) {
			t.Errorf("admin quota UI missing %q", marker)
		}
	}

	// Concatenated API expressions may wrap after formatting; the same endpoints and variables are required.
	for _, pattern := range []string{
		`"/admin/data/quota/"\s*\+\s*encodeURIComponent\(source\)\s*\+\s*"/sync"`,
		`"/admin/data/quota/"\s*\+\s*encodeURIComponent\(state\.editingQuotaClient\)`,
	} {
		if !regexp.MustCompile(pattern).MatchString(adminUIPageScripts(t, "quota")) {
			t.Errorf("quota.js missing API expression %s", pattern)
		}
	}
	// Quota behavior remains in quota.js; shared behavior is covered across all modules.
	for _, marker := range []string{
		`function quotaConvertedDetail(status)`,
		`data-model-binding-target`,
	} {
		if !strings.Contains(adminUIPageScripts(t, "quota"), marker) {
			t.Errorf("quota.js missing %q", marker)
		}
	}
	for _, marker := range []string{
		`id="quotaSyncSource"`,
		`function applyReadonlyMode(root)`,
		`await app.modules.users.loadUserInfo();`,
		`window.state = state;`,
		`window.isReadonlyMode = isReadonlyMode;`,
	} {
		if !strings.Contains(adminUIHTML+adminUIAllScripts(), marker) {
			t.Errorf("admin UI sources missing %q", marker)
		}
	}

	for _, src := range []string{adminUIHTML, adminUIAllScripts()} {
		if strings.Contains(src, `<option value="azure_openai">`) {
			t.Error("quota UI must not hardcode endpoint type options")
		}
	}
}
