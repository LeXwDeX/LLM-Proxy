"""release_gate_check.py 的改动行覆盖、文档联动与 FAKE 缺陷台账校验单测。"""
import io
import subprocess
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path

try:
    from scripts.testing import release_gate_check as gate
except ImportError:  # 直接运行本文件时，脚本目录在 sys.path 上
    import release_gate_check as gate

MODULE = 'github.com/LeXwDeX/LLM-Proxy'

DIFF = """diff --git a/internal/proxy/a.go b/internal/proxy/a.go
index 1..2 100644
--- a/internal/proxy/a.go
+++ b/internal/proxy/a.go
@@ -10,0 +11,2 @@ func x() {
+	a := 1
+	b := 2
@@ -30 +32 @@ func y() {
-	old()
+	fresh()
diff --git a/internal/proxy/a_test.go b/internal/proxy/a_test.go
new file mode 100644
--- /dev/null
+++ b/internal/proxy/a_test.go
@@ -0,0 +1,3 @@
+func TestAlpha(t *testing.T) {
+}
+func TestBeta(t *testing.T) {}
diff --git a/internal/proxy/gone.go b/internal/proxy/gone.go
deleted file mode 100644
--- a/internal/proxy/gone.go
+++ /dev/null
@@ -1,2 +0,0 @@
-x
-y
"""

LEDGER_OK = """# FAKE 缺陷台账

```
### FD-999 模板内的示例不参与解析
- 状态：bogus
```

### FD-001 示例已修复
- 阶段：TEST
- 状态：fixed
- 现象：fake 对某错误体返回 200
- 依据：厂商文档 2026-11-01
- 补充：presets.go 新增 preset
- 用例：TestSomethingE2E

### FD-002 示例已接受
- 阶段：DEV
- 状态：accepted
- 现象：fake 不模拟某长尾行为
- 依据：官方未示例
- 接受理由：真实上游验收覆盖，风险可控
"""


def block(path, start, end, count, stmts=1):
    return '{}/{}:{}.1,{}.10 {} {}'.format(MODULE, path, start, end, stmts, count)


class ParseDiffTests(unittest.TestCase):
    def test_added_lines_cover_pure_additions_and_replacements(self):
        added = gate.parse_added_lines(DIFF)
        self.assertEqual(added['internal/proxy/a.go'], [11, 12, 32])
        self.assertEqual(added['internal/proxy/a_test.go'], [1, 2, 3])

    def test_deleted_file_has_no_added_lines(self):
        self.assertNotIn('internal/proxy/gone.go', gate.parse_added_lines(DIFF))

    def test_added_test_functions(self):
        self.assertEqual(gate.parse_added_test_funcs(DIFF), ['TestAlpha', 'TestBeta'])


class CoverageTests(unittest.TestCase):
    def profile(self, *lines):
        return gate.parse_profile('mode: set\n' + '\n'.join(lines) + '\n', MODULE)

    def test_all_changed_lines_covered(self):
        blocks = self.profile(block('internal/proxy/a.go', 10, 13, 1))
        result = gate.diff_coverage({'internal/proxy/a.go': [11, 12]}, blocks)
        self.assertEqual(result.uncovered, {})
        self.assertEqual(result.missing_files, [])
        self.assertEqual(result.checked, 2)

    def test_zero_count_block_is_uncovered(self):
        blocks = self.profile(block('internal/proxy/a.go', 10, 13, 0))
        result = gate.diff_coverage({'internal/proxy/a.go': [11, 40]}, blocks)
        self.assertEqual(result.uncovered, {'internal/proxy/a.go': [11]})

    def test_line_without_statement_block_is_ignored(self):
        # 注释、签名行不在任何语句块内，不计入修改点。
        blocks = self.profile(block('internal/proxy/a.go', 10, 13, 1))
        result = gate.diff_coverage({'internal/proxy/a.go': [3]}, blocks)
        self.assertEqual(result.uncovered, {})
        self.assertEqual(result.checked, 0)

    def test_overlapping_blocks_any_zero_count_fails(self):
        blocks = self.profile(block('internal/proxy/a.go', 11, 11, 1), block('internal/proxy/a.go', 11, 12, 0))
        result = gate.diff_coverage({'internal/proxy/a.go': [11]}, blocks)
        self.assertEqual(result.uncovered, {'internal/proxy/a.go': [11]})

    def test_file_absent_from_profile_fails_closed(self):
        blocks = self.profile(block('internal/proxy/a.go', 1, 2, 1))
        result = gate.diff_coverage({'internal/quota/q.go': [5]}, blocks)
        self.assertEqual(result.missing_files, ['internal/quota/q.go'])

    def test_go_outside_profile_scope_is_manual_not_missing(self):
        blocks = self.profile(block('internal/proxy/a.go', 1, 2, 1))
        result = gate.diff_coverage({'scripts/tool/main.go': [3]}, blocks)
        self.assertEqual(result.missing_files, [])
        self.assertEqual(result.non_go, ['scripts/tool/main.go'])

    def test_test_files_and_non_go_are_not_checked(self):
        blocks = self.profile(block('internal/proxy/a.go', 1, 2, 1))
        added = {'internal/proxy/a_test.go': [1], 'README.md': [1], 'internal/admin/ui/x.js': [1]}
        result = gate.diff_coverage(added, blocks)
        self.assertEqual(result.missing_files, [])
        self.assertEqual(result.non_go, ['internal/admin/ui/x.js'])

    def test_malformed_profile_raises(self):
        with self.assertRaises(gate.GateError):
            gate.parse_profile('mode: set\nnot a block\n', MODULE)
        with self.assertRaises(gate.GateError):
            gate.parse_profile('', MODULE)

    def test_duplicate_blocks_keep_max_count(self):
        blocks = self.profile(block('internal/proxy/a.go', 10, 13, 0), block('internal/proxy/a.go', 10, 13, 2))
        result = gate.diff_coverage({'internal/proxy/a.go': [11]}, blocks)
        self.assertEqual(result.uncovered, {})


class DocsTests(unittest.TestCase):
    DOC = 'internal/proxy/new_feature_test.go 覆盖新特性'

    def run_docs(self, entries, doc=DOC):
        return gate.check_docs(entries, doc)

    def test_source_change_requires_regression_doc_update(self):
        findings = self.run_docs([('M', 'internal/proxy/forward.go')])
        self.assertTrue(any(f.startswith('R1') for f in findings), findings)

    def test_doc_update_clears_r1(self):
        findings = self.run_docs([('M', 'internal/proxy/forward.go'), ('M', gate.REGRESSION_DOC)])
        self.assertEqual([f for f in findings if f.startswith('R1')], [])

    def test_docs_only_change_needs_nothing(self):
        self.assertEqual(self.run_docs([('M', 'README.md')]), [])

    def test_new_test_file_must_be_named_in_doc(self):
        entries = [('M', gate.REGRESSION_DOC), ('A', 'internal/proxy/missing_test.go'),
                   ('A', 'internal/proxy/new_feature_test.go')]
        findings = self.run_docs(entries)
        self.assertEqual([f for f in findings if f.startswith('R2')],
                         ['R2 新增测试文件未在回归文档登记: internal/proxy/missing_test.go'])

    def test_basename_must_match_whole_token(self):
        entries = [('M', gate.REGRESSION_DOC), ('A', 'internal/proxy/a_test.go')]
        findings = self.run_docs(entries, doc='见 internal/proxy/data_a_test.go')
        self.assertEqual([f for f in findings if f.startswith('R2')],
                         ['R2 新增测试文件未在回归文档登记: internal/proxy/a_test.go'])
        self.assertEqual(self.run_docs(entries, doc='用例 a_test.go 覆盖'), [])

    def test_fakellm_source_change_requires_ledger(self):
        entries = [('M', 'internal/fakellm/presets.go'), ('M', gate.REGRESSION_DOC)]
        self.assertTrue(any(f.startswith('F1') for f in self.run_docs(entries)))
        entries.append(('M', gate.FAKE_LEDGER))
        self.assertEqual([f for f in self.run_docs(entries) if f.startswith('F1')], [])

    def test_fakellm_test_only_change_does_not_require_ledger(self):
        entries = [('M', 'internal/fakellm/server_test.go')]
        self.assertEqual([f for f in self.run_docs(entries) if f.startswith('F1')], [])


class LedgerTests(unittest.TestCase):
    def test_valid_ledger_passes_and_ignores_fenced_template(self):
        errors, entries = gate.lint_ledger(LEDGER_OK)
        self.assertEqual(errors, [])
        self.assertEqual([e.id for e in entries], ['FD-001', 'FD-002'])

    def test_fixed_requires_case_and_supplement(self):
        text = LEDGER_OK.replace('- 用例：TestSomethingE2E\n', '')
        errors, _ = gate.lint_ledger(text)
        self.assertTrue(any('FD-001' in e and '用例' in e for e in errors), errors)

    def test_accepted_requires_reason(self):
        text = LEDGER_OK.replace('- 接受理由：真实上游验收覆盖，风险可控\n', '')
        errors, _ = gate.lint_ledger(text)
        self.assertTrue(any('FD-002' in e and '接受理由' in e for e in errors), errors)

    def test_invalid_status_stage_and_duplicate_id(self):
        text = LEDGER_OK.replace('- 状态：fixed', '- 状态：done').replace('FD-002 示例', 'FD-001 重复')
        errors, _ = gate.lint_ledger(text)
        joined = '\n'.join(errors)
        self.assertIn('状态', joined)
        self.assertIn('重复', joined)

    def test_open_entry_is_ok_by_default_but_blocks_release(self):
        text = LEDGER_OK + '\n### FD-003 未修复\n- 阶段：回归\n- 状态：open\n- 现象：x\n- 依据：y\n'
        errors, entries = gate.lint_ledger(text)
        self.assertEqual(errors, [])
        self.assertEqual(gate.open_entries(entries), ['FD-003'])

    def test_missing_cases_reports_unknown_test_names(self):
        _, entries = gate.lint_ledger(LEDGER_OK)
        missing = gate.missing_cases(entries, lambda name: name != 'TestSomethingE2E')
        self.assertEqual(missing, ['FD-001 用例不存在: TestSomethingE2E'])
        self.assertEqual(gate.missing_cases(entries, lambda name: True), [])

    def test_empty_ledger_is_valid(self):
        errors, entries = gate.lint_ledger('# FAKE 缺陷台账\n\n暂无条目\n')
        self.assertEqual((errors, entries), ([], []))


class CliTests(unittest.TestCase):
    """用临时 git 仓库走通 docs 与 diff-coverage 子命令。"""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.repo = Path(self.tmp.name)
        self.git('init', '-q', '-b', 'main')
        self.git('config', 'user.email', 'gate@example.invalid')
        self.git('config', 'user.name', 'gate')
        (self.repo / 'go.mod').write_text('module ' + MODULE + '\n')
        self.write('internal/proxy/a.go', 'package proxy\n\nfunc A() int {\n\treturn 1\n}\n')
        self.write(gate.REGRESSION_DOC, '# doc\n')
        self.git('add', '-A')
        self.git('commit', '-q', '-m', 'base')
        self.git('tag', 'base')

    def git(self, *args):
        subprocess.run(['git', '-C', str(self.repo)] + list(args), check=True, capture_output=True)

    def write(self, rel, text):
        path = self.repo / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)

    def run_main(self, *arguments):
        out, err = io.StringIO(), io.StringIO()
        with redirect_stdout(out), redirect_stderr(err):
            code = gate.main(['--repo', str(self.repo)] + list(arguments))
        return code, out.getvalue(), err.getvalue()

    def test_docs_flags_source_change_without_doc(self):
        self.write('internal/proxy/a.go', 'package proxy\n\nfunc A() int {\n\treturn 2\n}\n')
        self.git('commit', '-qam', 'change')
        code, out, _ = self.run_main('docs', '--base', 'base')
        self.assertEqual(code, 1)
        self.assertIn('R1', out)

    def test_diff_coverage_reports_uncovered_changed_line(self):
        self.write('internal/proxy/a.go', 'package proxy\n\nfunc A() int {\n\treturn 2\n}\n')
        self.git('commit', '-qam', 'change')
        profile = self.repo / 'cover.out'
        profile.write_text('mode: set\n' + block('internal/proxy/a.go', 3, 5, 0) + '\n')
        code, out, _ = self.run_main('diff-coverage', '--base', 'base', '--profile', str(profile))
        self.assertEqual(code, 1)
        self.assertIn('internal/proxy/a.go:4', out)
        profile.write_text('mode: set\n' + block('internal/proxy/a.go', 3, 5, 1) + '\n')
        code, out, _ = self.run_main('diff-coverage', '--base', 'base', '--profile', str(profile))
        self.assertEqual(code, 0, out)

    def test_ledger_release_mode_blocks_open_entries(self):
        self.write('internal/proxy/x_test.go', 'package proxy\n\nfunc TestSomethingE2E(t *testing.T) {}\n')
        self.git('add', '-A')
        self.write(gate.FAKE_LEDGER, LEDGER_OK + '\n### FD-003 未修复\n- 阶段：回归\n- 状态：open\n- 现象：x\n- 依据：y\n')
        code, out, _ = self.run_main('ledger')
        self.assertEqual(code, 0, out)
        code, out, _ = self.run_main('ledger', '--release')
        self.assertEqual(code, 1)
        self.assertIn('FD-003', out)

    def test_non_ascii_path_is_not_skipped(self):
        self.write('internal/proxy/中文.go', 'package proxy\n\nfunc B() int {\n\treturn 1\n}\n')
        self.git('add', '-A')
        self.git('commit', '-qm', 'non-ascii')
        profile = self.repo / 'cover.out'
        profile.write_text('mode: set\n' + block('internal/proxy/a.go', 3, 5, 1) + '\n')
        code, out, _ = self.run_main('diff-coverage', '--base', 'base', '--profile', str(profile))
        self.assertEqual(code, 1, out)
        self.assertIn('NOT_IN_PROFILE internal/proxy/中文.go', out)

    def test_ledger_release_checks_fixed_case_exists_in_repo(self):
        self.write(gate.FAKE_LEDGER, LEDGER_OK)
        code, out, _ = self.run_main('ledger')
        self.assertEqual(code, 1)
        self.assertIn('TestSomethingE2E', out)
        self.write('internal/proxy/x_test.go', 'package proxy\n\nfunc TestSomethingE2E(t *testing.T) {}\n')
        self.git('add', '-A')
        code, out, _ = self.run_main('ledger')
        self.assertEqual(code, 0, out)

    def test_bad_base_exits_2(self):
        code, _, err = self.run_main('docs', '--base', 'no-such-ref')
        self.assertEqual(code, 2)
        self.assertIn('no-such-ref', err)


if __name__ == '__main__':
    unittest.main()
