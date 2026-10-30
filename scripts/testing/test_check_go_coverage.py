"""check_go_coverage.py 的解析、汇总与阈值退出码单测。"""
import io
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path

try:
    from scripts.testing import check_go_coverage
except ImportError:  # 直接运行本文件时，脚本目录在 sys.path 上
    import check_go_coverage

ADMIN = 'github.com/LeXwDeX/LLM-Proxy/internal/admin/handlers.go'
ADMIN_UI = 'github.com/LeXwDeX/LLM-Proxy/internal/adminui/handlers.go'
PROXY = 'github.com/LeXwDeX/LLM-Proxy/internal/proxy/forward.go'


def cover_line(path, statements=1, count=1, line=1):
    """构造一条覆盖块；同一 path+line 视为同一块。"""
    return '{}:{}.1,{}.10 {} {}'.format(path, line, line, statements, count)


class CoverageCheckTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.profile = Path(self.tmp.name) / 'coverage.out'

    def write_profile(self, *lines):
        self.profile.write_text('\n'.join(lines) + '\n')

    def run_main(self, *arguments):
        stdout, stderr = io.StringIO(), io.StringIO()
        with redirect_stdout(stdout), redirect_stderr(stderr):
            code = check_go_coverage.main(['--profile', str(self.profile)] + list(arguments))
        return code, stdout.getvalue(), stderr.getvalue()

    def two_package_profile(self):
        # admin 2/2 已覆盖，proxy 1/2，总计 3/4 = 75.0%。
        self.write_profile(
            'mode: set',
            cover_line(ADMIN, line=1),
            cover_line(ADMIN, line=5),
            cover_line(PROXY, line=1, count=1),
            cover_line(PROXY, line=5, count=0),
        )

    def test_thresholds_at_boundary_pass_and_print_sorted_table(self):
        self.two_package_profile()
        code, stdout, stderr = self.run_main(
            '--min-total', '75',
            '--min-package', 'internal/admin=100',
            '--min-package', 'internal/proxy=50',
        )
        self.assertEqual(code, 0, stderr)
        self.assertIn('coverage check passed', stdout)
        self.assertIn('TOTAL', stdout)
        self.assertIn('75.0%', stdout)
        self.assertIn('100.0%', stdout)
        self.assertIn('50.0%', stdout)
        # 输出按包名排序，与 profile 中的出现顺序无关。
        self.assertLess(stdout.index('internal/admin'), stdout.index('internal/proxy'))

    def test_total_below_threshold_fails(self):
        self.two_package_profile()
        code, _, stderr = self.run_main('--min-total', '75.1')
        self.assertEqual(code, 1)
        self.assertIn('total coverage 75.0% is below threshold 75.1%', stderr)

    def test_package_below_threshold_fails(self):
        self.two_package_profile()
        code, _, stderr = self.run_main('--min-total', '0', '--min-package', 'internal/proxy=50.1')
        self.assertEqual(code, 1)
        self.assertIn(
            'package github.com/LeXwDeX/LLM-Proxy/internal/proxy',
            stderr,
        )
        self.assertIn('50.0% is below threshold 50.1%', stderr)

    def test_missing_package_fails(self):
        self.two_package_profile()
        code, _, stderr = self.run_main('--min-total', '0', '--min-package', 'internal/nosql=80')
        self.assertEqual(code, 1)
        self.assertIn('package not found in profile: internal/nosql', stderr)

    def test_package_matching_respects_directory_boundary(self):
        # internal/admin 不能误匹配 internal/adminui。
        self.write_profile('mode: set', cover_line(ADMIN_UI, count=1))
        code, _, stderr = self.run_main('--min-package', 'internal/admin=80')
        self.assertEqual(code, 1)
        self.assertIn('package not found in profile: internal/admin', stderr)

        # 两个目录同时存在时，只对目录边界完整匹配的包应用阈值。
        self.write_profile(
            'mode: set',
            cover_line(ADMIN, count=1),
            cover_line(ADMIN_UI, count=0),
        )
        code, stdout, stderr = self.run_main(
            '--min-total', '0', '--min-package', 'internal/admin=100',
        )
        self.assertEqual(code, 0, stderr)
        self.assertIn('TOTAL', stdout)

    def test_total_uses_statement_counts_not_block_counts(self):
        # 2/3 覆盖：按语句数而非块数计算，总计 66.7%。
        self.write_profile(
            'mode: count',
            cover_line(ADMIN, statements=2, count=1),
            cover_line(PROXY, statements=1, count=0),
        )
        code, stdout, stderr = self.run_main()
        self.assertEqual(code, 1)
        self.assertIn('total coverage 66.7% is below threshold 80.0%', stderr)
        totals = check_go_coverage.parse_profile(self.profile)
        grand = sum((totals[name].covered for name in totals), 0)
        statements = sum((totals[name].statements for name in totals), 0)
        self.assertEqual((grand, statements), (2, 3))
        self.assertAlmostEqual(100.0 * grand / statements, 66.666666, places=5)

    def test_count_greater_than_zero_is_covered(self):
        self.write_profile(
            'mode: set',
            cover_line(ADMIN, line=1, count=0),
            cover_line(ADMIN, line=5, count=1),
            cover_line(ADMIN, line=9, count=7),
        )
        totals = check_go_coverage.parse_profile(self.profile)
        admin = totals['github.com/LeXwDeX/LLM-Proxy/internal/admin']
        self.assertEqual(admin.statements, 3)
        self.assertEqual(admin.covered, 2)
        self.assertAlmostEqual(admin.percent, 66.666666, places=5)

    def test_duplicate_blocks_take_maximum_count_and_count_statements_once(self):
        self.write_profile(
            'mode: count',
            cover_line(ADMIN, statements=3, count=0),
            cover_line(ADMIN, statements=3, count=2),
            cover_line(PROXY, statements=1, count=0),
        )
        totals = check_go_coverage.parse_profile(self.profile)
        admin = totals['github.com/LeXwDeX/LLM-Proxy/internal/admin']
        self.assertEqual(admin.statements, 3)
        self.assertEqual(admin.covered, 3)
        proxy = totals['github.com/LeXwDeX/LLM-Proxy/internal/proxy']
        self.assertEqual((proxy.statements, proxy.covered), (1, 0))

    def test_duplicate_uncovered_blocks_stay_uncovered(self):
        self.write_profile(
            'mode: set',
            cover_line(ADMIN, statements=2, count=0),
            cover_line(ADMIN, statements=2, count=0),
        )
        totals = check_go_coverage.parse_profile(self.profile)
        admin = totals['github.com/LeXwDeX/LLM-Proxy/internal/admin']
        self.assertEqual((admin.statements, admin.covered), (2, 0))
        self.assertEqual(admin.percent, 0.0)

    def test_duplicate_blocks_with_conflicting_statement_counts_are_rejected(self):
        self.write_profile(
            'mode: set',
            cover_line(ADMIN, statements=1, count=1),
            cover_line(ADMIN, statements=2, count=1),
        )
        code, _, stderr = self.run_main()
        self.assertEqual(code, 2)
        self.assertIn('conflicting statement counts', stderr)

    def test_all_supported_modes_are_accepted(self):
        for mode in ('set', 'count', 'atomic'):
            with self.subTest(mode=mode):
                self.write_profile('mode: ' + mode, cover_line(ADMIN))
                totals = check_go_coverage.parse_profile(self.profile)
                admin = totals['github.com/LeXwDeX/LLM-Proxy/internal/admin']
                self.assertEqual((admin.statements, admin.covered), (1, 1))

    def test_empty_profile_is_rejected(self):
        self.profile.write_text('')
        code, _, stderr = self.run_main()
        self.assertEqual(code, 2)
        self.assertIn('is empty', stderr)

    def test_profile_without_mode_line_is_rejected(self):
        self.write_profile(cover_line(ADMIN))
        code, _, stderr = self.run_main()
        self.assertEqual(code, 2)
        self.assertIn('must start with a mode line', stderr)

    def test_unsupported_mode_is_rejected(self):
        self.write_profile('mode: bogus', cover_line(ADMIN))
        code, _, stderr = self.run_main()
        self.assertEqual(code, 2)
        self.assertIn('unsupported mode', stderr)

    def test_profile_with_only_mode_line_is_rejected(self):
        self.write_profile('mode: set')
        code, _, stderr = self.run_main()
        self.assertEqual(code, 2)
        self.assertIn('contains no coverage blocks', stderr)

    def test_malformed_block_line_is_rejected(self):
        self.write_profile('mode: set', 'not a coverage block')
        code, _, stderr = self.run_main()
        self.assertEqual(code, 2)
        self.assertIn('invalid coverage block', stderr)

    def test_zero_statement_blocks_are_ignored(self):
        # 真实 Go profile 包含空块（numStmts=0），不能因此拒绝整个 profile。
        self.write_profile(
            'mode: set',
            cover_line(ADMIN, statements=0, count=0, line=1),
            cover_line(ADMIN, statements=0, count=1, line=5),
            cover_line(ADMIN, statements=2, count=1, line=9),
        )
        code, stdout, stderr = self.run_main('--min-total', '100', '--min-package', 'internal/admin=100')
        self.assertEqual(code, 0, stderr)
        self.assertIn('TOTAL', stdout)
        admin = check_go_coverage.parse_profile(self.profile)['github.com/LeXwDeX/LLM-Proxy/internal/admin']
        self.assertEqual((admin.statements, admin.covered), (2, 2))

    def test_missing_profile_file_is_rejected(self):
        missing = Path(self.tmp.name) / 'missing.out'
        stdout, stderr = io.StringIO(), io.StringIO()
        with redirect_stdout(stdout), redirect_stderr(stderr):
            code = check_go_coverage.main(['--profile', str(missing)])
        self.assertEqual(code, 2)
        self.assertIn('cannot read profile', stderr.getvalue())

    def test_invalid_thresholds_are_rejected(self):
        self.two_package_profile()
        for arguments in (
            ('--min-total', 'abc'),
            ('--min-total', '101'),
            ('--min-total', '-1'),
            ('--min-total', '0', '--min-package', 'internal/admin'),
            ('--min-total', '0', '--min-package', '=80'),
            ('--min-total', '0', '--min-package', 'internal/admin=x'),
        ):
            with self.subTest(arguments=arguments):
                code, _, stderr = self.run_main(*arguments)
                self.assertEqual(code, 2)
                self.assertTrue(stderr.strip(), 'expected a diagnostic on stderr')


if __name__ == '__main__':
    unittest.main()
