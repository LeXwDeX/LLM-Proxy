#!/usr/bin/env python3
"""
解析 Go cover profile，按包统计语句覆盖率并强制阈值。

用法:
  go test -coverprofile=coverage.out ./internal/...
  python3 scripts/testing/check_go_coverage.py --profile coverage.out \\
      --min-total 80 --min-package internal/admin=80

profile 行格式:
  mode: set|count|atomic
  <import/path/file.go>:<startLine>.<startCol>,<endLine>.<endCol> <numStmts> <count>

只支持单次 `go test -coverprofile` 生成的 profile（单一 mode 行）。合并 profile
或多个 mode 段不属于支持范围，按损坏处理并以退出码 2 结束。

同一 profile 内同一块重复出现时按块键去重并取最大 count，语句数只累计一次；
count>0 记为已覆盖。

--min-package 只匹配精确的包目录：完整导入路径等于 PKG 或以 "/PKG" 结尾，
不并入其子包（例如 internal/admin 不匹配 internal/admin/ui）。

退出码:
  0  所有阈值满足
  1  覆盖率低于阈值，或指定的包在 profile 中不存在
  2  profile 为空、格式错误（含非 UTF-8、多 mode 段、计数超限）或命令行参数非法
"""

from __future__ import annotations

import argparse
import re
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, Iterable, List, Tuple

# 一条覆盖块：文件路径、起止位置、语句数、命中次数。
BLOCK_RE = re.compile(
    r"^(?P<file>.+):(?P<start_line>\d+)\.(?P<start_col>\d+),"
    r"(?P<end_line>\d+)\.(?P<end_col>\d+) (?P<statements>\d+) (?P<count>\d+)$"
)
VALID_MODES = {"set", "count", "atomic"}

# Go 编译器产出的单块语句数和命中次数远小于该上限；超过即视为损坏 profile，
# 同时保证 Totals.percent 的浮点运算不会因超大整数溢出成 inf。
MAX_BLOCK_COUNT = 1_000_000_000

BlockKey = Tuple[str, int, int, int, int]


class ProfileError(Exception):
    """profile 内容或命令行参数不合法。"""


@dataclass
class Totals:
    """一个包（或全局）的语句统计。"""

    statements: int = 0
    covered: int = 0

    def add(self, statements: int, count: int) -> None:
        self.statements += statements
        if count > 0:
            self.covered += statements

    def merge(self, other: "Totals") -> None:
        """合并已统计好的包级 Totals，不能把 covered 当作执行次数。"""
        self.statements += other.statements
        self.covered += other.covered

    @property
    def percent(self) -> float:
        if self.statements == 0:
            return 0.0
        return 100.0 * self.covered / self.statements


def package_of(file_path: str) -> str:
    """按文件所在目录归组；目录路径即 Go 包路径。"""
    directory, separator, _name = file_path.rpartition("/")
    return directory if separator else "."


def parse_block_number(
    path: Path,
    line_number: int,
    field: str,
    text: str,
    limit: int | None = None,
) -> int:
    """解析块行中的数字字段；位数超限或超过约定上限按损坏 profile 处理。"""
    try:
        value = int(text)
    except (ValueError, OverflowError) as exc:
        # CPython 对超过 sys.get_int_max_str_digits()（默认 4300 位）的十进制字面量
        # 抛 ValueError；同时捕获 OverflowError 以覆盖其他解释器的同类失败。
        raise ProfileError(
            f"profile {path} line {line_number}: {field} {text[:32]!r} "
            f"is not a valid integer: {exc}"
        ) from exc
    if limit is not None and value > limit:
        raise ProfileError(
            f"profile {path} line {line_number}: {field} {value} "
            f"exceeds the maximum supported value {limit}"
        )
    return value


def parse_profile(path: Path) -> Dict[str, Totals]:
    """读取并解析 cover profile，返回按包累计的覆盖率。"""
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise ProfileError(f"cannot read profile {path}: {exc}") from exc
    except UnicodeError as exc:
        raise ProfileError(f"profile {path} is not valid UTF-8: {exc}") from exc

    lines = text.splitlines()
    if not lines or not lines[0].strip():
        raise ProfileError(f"profile {path} is empty")
    header = lines[0].strip()
    if not header.startswith("mode:"):
        raise ProfileError(f"profile {path} must start with a mode line")
    mode = header[len("mode:") :].strip()
    if mode not in VALID_MODES:
        raise ProfileError(
            f"profile {path} has unsupported mode {mode!r}; expected one of {sorted(VALID_MODES)}"
        )

    # 按块键去重：重复块只计一次语句数，命中次数取最大值。
    blocks: Dict[BlockKey, Tuple[int, int]] = {}
    for line_number, raw_line in enumerate(lines[1:], start=2):
        line = raw_line.strip()
        if not line:
            continue
        if line.startswith("mode:"):
            raise ProfileError(
                f"profile {path} line {line_number}: multiple mode lines are not supported; "
                "expected a profile from a single go test run"
            )
        match = BLOCK_RE.match(line)
        if match is None:
            raise ProfileError(f"profile {path} line {line_number}: invalid coverage block {line!r}")
        file_path = match.group("file")
        statements = parse_block_number(
            path, line_number, "statements", match.group("statements"), limit=MAX_BLOCK_COUNT
        )
        count = parse_block_number(
            path, line_number, "count", match.group("count"), limit=MAX_BLOCK_COUNT
        )
        # Go 会为没有语句的块（如空函数体、右花括号）输出 numStmts=0，需接受并忽略。
        key: BlockKey = (
            file_path,
            parse_block_number(path, line_number, "start_line", match.group("start_line")),
            parse_block_number(path, line_number, "start_col", match.group("start_col")),
            parse_block_number(path, line_number, "end_line", match.group("end_line")),
            parse_block_number(path, line_number, "end_col", match.group("end_col")),
        )
        previous = blocks.get(key)
        if previous is None:
            blocks[key] = (statements, count)
            continue
        # 同一块的语句数由源码决定，重复行必须一致；不一致说明 profile 已损坏。
        if previous[0] != statements:
            raise ProfileError(
                f"profile {path} line {line_number}: duplicate block {file_path} "
                f"has conflicting statement counts {previous[0]} and {statements}"
            )
        blocks[key] = (statements, max(previous[1], count))

    if not blocks:
        raise ProfileError(f"profile {path} contains no coverage blocks")

    totals: Dict[str, Totals] = {}
    for (file_path, *_rest), (statements, count) in blocks.items():
        totals.setdefault(package_of(file_path), Totals()).add(statements, count)
    return totals


def parse_percent(value: str, label: str) -> float:
    """解析 0–100 的百分比。"""
    try:
        percent = float(value)
    except ValueError as exc:
        raise ProfileError(f"{label} must be a number, got {value!r}") from exc
    if not 0.0 <= percent <= 100.0:
        raise ProfileError(f"{label} must be between 0 and 100, got {percent}")
    return percent


def parse_min_package(value: str) -> Tuple[str, float]:
    """解析 PKG=PERCENT。"""
    package, separator, percent_text = value.rpartition("=")
    if not separator or not package.strip():
        raise ProfileError(f"--min-package must be PKG=PERCENT, got {value!r}")
    return package.strip(), parse_percent(percent_text, f"threshold for {package.strip()}")


def matching_packages(totals: Dict[str, Totals], spec: str) -> List[str]:
    """按目录边界匹配包名，例如 internal/admin 匹配完整导入路径的末段。"""
    return sorted(package for package in totals if package == spec or package.endswith("/" + spec))


def print_table(totals: Dict[str, Totals]) -> None:
    """按包名排序输出覆盖率表与总计。"""
    grand = Totals()
    width = max([len("package")] + [len(package) for package in totals])
    print(f"{'package':<{width}} {'covered':>8} {'statements':>11} {'coverage':>9}")
    for package in sorted(totals):
        package_totals = totals[package]
        grand.merge(package_totals)
        print(
            f"{package:<{width}} {package_totals.covered:>8} "
            f"{package_totals.statements:>11} {package_totals.percent:>8.2f}%"
        )
    print(
        f"{'TOTAL':<{width}} {grand.covered:>8} "
        f"{grand.statements:>11} {grand.percent:>8.2f}%"
    )


def evaluate(
    totals: Dict[str, Totals],
    min_total: float,
    min_packages: Iterable[Tuple[str, float]],
) -> List[str]:
    """返回全部未满足的阈值说明；空列表表示通过。"""
    failures: List[str] = []
    for package, minimum in min_packages:
        matched = matching_packages(totals, package)
        if not matched:
            failures.append(f"package not found in profile: {package}")
            continue
        for name in matched:
            actual = totals[name]
            if actual.percent < minimum:
                failures.append(
                    f"package {name} coverage {actual.percent:.2f}% "
                    f"({actual.covered}/{actual.statements} statements) "
                    f"is below threshold {minimum:.2f}%"
                )

    grand = Totals()
    for package_totals in totals.values():
        grand.merge(package_totals)
    if grand.percent < min_total:
        failures.append(
            f"total coverage {grand.percent:.2f}% "
            f"({grand.covered}/{grand.statements} statements) "
            f"is below threshold {min_total:.2f}%"
        )
    return failures


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description=(
            "Parse a Go cover profile from a single `go test -coverprofile` run "
            "and enforce per-package/total coverage thresholds. Profiles with "
            "multiple mode lines are rejected as corrupt."
        )
    )
    parser.add_argument(
        "--profile",
        required=True,
        help="path to a Go cover profile from a single go test run",
    )
    parser.add_argument(
        "--min-total",
        default="80",
        metavar="PERCENT",
        help="minimum total statement coverage (default: 80)",
    )
    parser.add_argument(
        "--min-package",
        action="append",
        default=[],
        metavar="PKG=PERCENT",
        help=(
            "minimum coverage for an exact package directory (repeatable); "
            "subpackages are not included"
        ),
    )
    return parser


def main(argv: Iterable[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        min_total = parse_percent(args.min_total, "--min-total")
        min_packages = [parse_min_package(value) for value in args.min_package]
        totals = parse_profile(Path(args.profile))
    except ProfileError as exc:
        print(f"coverage check error: {exc}", file=sys.stderr)
        return 2

    print_table(totals)
    failures = evaluate(totals, min_total, min_packages)
    if failures:
        for failure in failures:
            print(f"coverage check failed: {failure}", file=sys.stderr)
        return 1
    print("coverage check passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
