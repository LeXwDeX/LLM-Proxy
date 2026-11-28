#!/usr/bin/env python3
"""
发布门禁的确定性检查：改动行覆盖、回归文档联动、FAKE 缺陷台账。

门禁 agent 负责判断与取证，凡能机械判定的事实交给本脚本，避免凭印象放行。

子命令（均以 --repo 指定仓库根，默认当前目录；--base 为上一次发布的 tag 或提交）:

  diff-coverage --base REF --profile coverage.out
      本次改动（base...HEAD）新增或修改的每一行 Go 非测试语句都必须落在
      命中次数大于 0 的覆盖块内。profile 由 go test -coverprofile 生成，
      只支持单一 mode 行。不在任何语句块内的行（注释、签名、花括号）不计入；
      改动文件整体不在 profile 中视为未覆盖（fail closed）。非 Go 代码文件
      （管理前端 JS 等）无自动覆盖数据，只列出供人工逐项核对。
  docs --base REF
      R1 非测试源码有改动时，回归文档必须同次更新；
      R2 新增的测试文件必须在回归文档中登记（路径或文件名出现即可）；
      F1 fakellm 非测试文件有改动时，FAKE 缺陷台账必须同次更新。
      另列出新增但回归文档未提及的 Test 函数名，仅作提示，不影响退出码。
  ledger [--release]
      校验 FAKE 缺陷台账格式，并核对 fixed 条目“用例”中的 Test 函数真实存在；
      --release 时还要求没有状态为 open 的条目。

退出码: 0 通过；1 有未满足的门禁项；2 参数、git、profile 或文件错误。
"""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Dict, List, Optional, Sequence, Set, Tuple

REGRESSION_DOC = "scripts/testing/README.md"
FAKE_LEDGER = "internal/fakellm/DEFECTS.md"

BLOCK_RE = re.compile(
    r"^(?P<file>.+):(?P<sl>\d+)\.(?P<sc>\d+),(?P<el>\d+)\.(?P<ec>\d+) "
    r"(?P<stmts>\d+) (?P<count>\d+)$"
)
HUNK_RE = re.compile(r"^@@ -\d+(?:,\d+)? \+(?P<start>\d+)(?:,(?P<len>\d+))? @@")
TEST_FUNC_RE = re.compile(r"^\+func (Test\w+)\(")
PROFILE_SCOPE = ("internal/", "cmd/")
NON_GO_CODE_EXT = {".js", ".cjs", ".mjs", ".html", ".css", ".py", ".sh"}

STAGES = {"DEV", "TEST", "回归"}
STATUSES = {"open", "fixed", "accepted"}
REQUIRED_FIELDS = ("阶段", "状态", "现象", "依据")
FIELD_NAMES = REQUIRED_FIELDS + ("补充", "用例", "接受理由")
HEADING_RE = re.compile(r"^###\s+(?P<id>FD-\S+)\s*(?P<title>.*)$")
FIELD_RE = re.compile(r"^-\s*(?P<name>" + "|".join(FIELD_NAMES) + r")\s*[：:]\s*(?P<value>.*)$")


class GateError(Exception):
    """输入、git 或文件错误；与“门禁未通过”区分，退出码 2。"""


# ---------------------------------------------------------------- 路径分类

def is_test_file(path: str) -> bool:
    name = path.rsplit("/", 1)[-1]
    return (
        name.endswith("_test.go")
        or (name.startswith("test_") and name.endswith(".py"))
        or ".test." in name
        or name.endswith(("_test.cjs", "_test.js"))
        or "/testdata/" in path
    )


def is_go_source(path: str) -> bool:
    return path.endswith(".go") and not is_test_file(path)


def is_non_go_code(path: str) -> bool:
    return any(path.endswith(ext) for ext in NON_GO_CODE_EXT) and not is_test_file(path)


def is_source_change(path: str) -> bool:
    if is_go_source(path):
        return path.startswith(("internal/", "cmd/", "scripts/"))
    if is_non_go_code(path):
        return path.startswith(("internal/admin/ui/", "scripts/"))
    return False


def is_fake_change(path: str) -> bool:
    if is_test_file(path):
        return False
    return path.startswith(("internal/fakellm/", "cmd/fakellm/")) or path == "Dockerfile.fakellm"


# ---------------------------------------------------------------- diff 解析

def parse_added_lines(diff_text: str) -> Dict[str, List[int]]:
    """解析 `git diff -U0` 输出，返回每个文件新增或修改后的行号。"""
    result: Dict[str, List[int]] = {}
    current: Optional[str] = None
    for line in diff_text.splitlines():
        if line.startswith("+++ "):
            target = line[4:].split("\t")[0]
            current = target[2:] if target.startswith("b/") else None
            continue
        match = HUNK_RE.match(line)
        if match and current:
            start = int(match.group("start"))
            length = 1 if match.group("len") is None else int(match.group("len"))
            if length:
                result.setdefault(current, []).extend(range(start, start + length))
    return result


def parse_added_test_funcs(diff_text: str) -> List[str]:
    names: List[str] = []
    for line in diff_text.splitlines():
        match = TEST_FUNC_RE.match(line)
        if match and match.group(1) not in names:
            names.append(match.group(1))
    return names


# ---------------------------------------------------------------- 覆盖

Block = Tuple[int, int, int, int, int, int]  # start_line, end_line, statements, count, start_col, end_col


def parse_profile(text: str, module: str) -> Dict[str, List[Block]]:
    lines = [line for line in text.splitlines() if line.strip()]
    if not lines or not lines[0].startswith("mode: "):
        raise GateError("coverage profile 为空或缺少 mode 行")
    merged: Dict[Tuple[str, int, int, int, int], Block] = {}
    prefix = module + "/"
    for line in lines[1:]:
        match = BLOCK_RE.match(line)
        if not match:
            raise GateError("coverage profile 含无法解析的行: " + line[:80])
        file = match.group("file")
        if file.startswith(prefix):
            file = file[len(prefix):]
        key = (file, int(match.group("sl")), int(match.group("sc")), int(match.group("el")), int(match.group("ec")))
        count = int(match.group("count"))
        old = merged.get(key)
        merged[key] = (key[1], key[3], int(match.group("stmts")), max(count, old[3]) if old else count, key[2], key[4])
    blocks: Dict[str, List[Block]] = {}
    for key, block in merged.items():
        blocks.setdefault(key[0], []).append(block)
    return blocks


@dataclass
class CoverageResult:
    uncovered: Dict[str, List[int]] = field(default_factory=dict)
    missing_files: List[str] = field(default_factory=list)
    non_go: List[str] = field(default_factory=list)
    checked: int = 0


def reaches_only_closing_brace(block: Block, line: int, text: Optional[str]) -> bool:
    """多行块在末行只到达行首的 `}`（如 `} else if y {`），该行没有属于这个块的语句。"""
    if text is None or block[1] != line or block[0] == line:
        return False
    stripped = text.lstrip()
    indent = len(text) - len(stripped)
    return stripped.startswith("}") and block[5] <= indent + 2


def diff_coverage(added: Dict[str, List[int]], blocks: Dict[str, List[Block]], line_text=None) -> CoverageResult:
    """line_text(path, line) 返回候选提交中该行的源码，用于识别只含闭合花括号的块边界；省略时不做该豁免。"""
    result = CoverageResult()
    for path in sorted(added):
        if is_non_go_code(path):
            result.non_go.append(path)
            continue
        if not is_go_source(path):
            continue
        if not path.startswith(PROFILE_SCOPE):
            # profile 只覆盖 internal/ 与 cmd/，其余 Go 文件无自动覆盖数据
            result.non_go.append(path)
            continue
        if path not in blocks:
            result.missing_files.append(path)
            continue
        for line in sorted(set(added[path])):
            text = line_text(path, line) if line_text else None
            covering = [b for b in blocks[path]
                        if b[0] <= line <= b[1] and b[2] > 0 and not reaches_only_closing_brace(b, line, text)]
            if not covering:
                continue
            result.checked += 1
            if any(b[3] == 0 for b in covering):
                result.uncovered.setdefault(path, []).append(line)
    return result


def format_ranges(lines: Sequence[int]) -> List[str]:
    ranges: List[str] = []
    start = prev = None
    for line in sorted(lines):
        if start is None:
            start = prev = line
        elif line == prev + 1:
            prev = line
        else:
            ranges.append(str(start) if start == prev else "{}-{}".format(start, prev))
            start = prev = line
    if start is not None:
        ranges.append(str(start) if start == prev else "{}-{}".format(start, prev))
    return ranges


# ---------------------------------------------------------------- 文档联动

def mentions(doc_text: str, path: str) -> bool:
    """路径整体出现，或文件名作为完整词出现（data_a_test.go 不算提到 a_test.go）。"""
    if path in doc_text:
        return True
    name = re.escape(path.rsplit("/", 1)[-1])
    return re.search(r"(?<![\w])" + name + r"(?!\w)", doc_text) is not None


def check_docs(entries: Sequence[Tuple[str, str]], doc_text: str) -> List[str]:
    findings: List[str] = []
    paths = {path for _, path in entries}
    sources = sorted(p for p in paths if is_source_change(p))
    if sources and REGRESSION_DOC not in paths:
        findings.append("R1 源码有改动但回归文档 {} 未更新（例: {}）".format(REGRESSION_DOC, sources[0]))
    for status, path in sorted(entries, key=lambda e: e[1]):
        if status == "A" and is_test_file(path) and path != REGRESSION_DOC:
            if not mentions(doc_text, path):
                findings.append("R2 新增测试文件未在回归文档登记: " + path)
    fakes = sorted(p for p in paths if is_fake_change(p))
    if fakes and FAKE_LEDGER not in paths:
        findings.append("F1 fakellm 有改动但 FAKE 缺陷台账 {} 未更新（例: {}）".format(FAKE_LEDGER, fakes[0]))
    return findings


# ---------------------------------------------------------------- 台账

@dataclass
class LedgerEntry:
    id: str
    title: str
    fields: Dict[str, str] = field(default_factory=dict)


def parse_ledger(text: str) -> List[LedgerEntry]:
    entries: List[LedgerEntry] = []
    fenced = False
    for line in text.splitlines():
        if line.strip().startswith("```"):
            fenced = not fenced
            continue
        if fenced:
            continue
        heading = HEADING_RE.match(line)
        if heading:
            entries.append(LedgerEntry(heading.group("id"), heading.group("title").strip()))
            continue
        item = FIELD_RE.match(line)
        if item and entries:
            entries[-1].fields[item.group("name")] = item.group("value").strip()
    return entries


def lint_ledger(text: str) -> Tuple[List[str], List[LedgerEntry]]:
    entries = parse_ledger(text)
    errors: List[str] = []
    seen: Set[str] = set()
    for entry in entries:
        if not re.fullmatch(r"FD-\d{3}", entry.id):
            errors.append("{} 编号格式应为 FD-三位数字".format(entry.id))
        if entry.id in seen:
            errors.append("{} 编号重复".format(entry.id))
        seen.add(entry.id)
        for name in REQUIRED_FIELDS:
            if not entry.fields.get(name):
                errors.append("{} 缺少字段「{}」".format(entry.id, name))
        if entry.fields.get("阶段") and entry.fields["阶段"] not in STAGES:
            errors.append("{} 阶段取值须为 {}".format(entry.id, "/".join(sorted(STAGES))))
        status = entry.fields.get("状态")
        if status and status not in STATUSES:
            errors.append("{} 状态取值须为 {}".format(entry.id, "/".join(sorted(STATUSES))))
        if status == "fixed":
            for name in ("补充", "用例"):
                if not entry.fields.get(name):
                    errors.append("{} 缺少字段「{}」（状态=fixed 必填）".format(entry.id, name))
        if status == "accepted" and not entry.fields.get("接受理由"):
            errors.append("{} 缺少字段「接受理由」（状态=accepted 必填）".format(entry.id))
    return errors, entries


def missing_cases(entries: Sequence[LedgerEntry], exists) -> List[str]:
    """fixed 条目“用例”字段中的 Test 函数名必须真实存在；exists(name) 由调用方提供。"""
    errors: List[str] = []
    for entry in entries:
        if entry.fields.get("状态") != "fixed":
            continue
        names = re.findall(r"\bTest\w+", entry.fields.get("用例", ""))
        if not names:
            errors.append("{} 用例须包含 Test 函数名".format(entry.id))
        for name in names:
            if not exists(name):
                errors.append("{} 用例不存在: {}".format(entry.id, name))
    return errors


def open_entries(entries: Sequence[LedgerEntry]) -> List[str]:
    return [e.id for e in entries if e.fields.get("状态") == "open"]


# ---------------------------------------------------------------- git 与命令行

def git(repo: Path, *args: str) -> str:
    proc = subprocess.run(["git", "-C", str(repo), "-c", "core.quotepath=false", "-c", "color.ui=false", "-c", "color.diff=false"] + list(args), capture_output=True, text=True)
    if proc.returncode != 0:
        raise GateError("git {} 失败: {}".format(" ".join(args), proc.stderr.strip()))
    return proc.stdout


def resolve_base(repo: Path, base: str) -> str:
    try:
        return git(repo, "rev-parse", "--verify", base + "^{commit}").strip()
    except GateError:
        raise GateError("找不到基线引用 " + base)


def module_path(repo: Path) -> str:
    gomod = repo / "go.mod"
    if not gomod.is_file():
        raise GateError("缺少 go.mod: " + str(gomod))
    match = re.search(r"^module\s+(\S+)", gomod.read_text(encoding="utf-8"), re.M)
    if not match:
        raise GateError("go.mod 缺少 module 行")
    return match.group(1)


def read_doc(repo: Path, path: str) -> str:
    return git(repo, "show", "HEAD:" + path)


def cmd_diff_coverage(repo: Path, args: argparse.Namespace) -> int:
    base = resolve_base(repo, args.base)
    diff = git(repo, "diff", "-U0", "--no-renames", "--diff-filter=AM", base + "...HEAD")
    try:
        text = Path(args.profile).read_text(encoding="utf-8")
    except OSError as exc:
        raise GateError("无法读取 profile: {}".format(exc))
    sources: Dict[str, List[str]] = {}

    def line_text(path: str, line: int) -> Optional[str]:
        if path not in sources:
            sources[path] = git(repo, "show", "HEAD:" + path).splitlines()
        lines = sources[path]
        return lines[line - 1] if 0 < line <= len(lines) else None

    result = diff_coverage(parse_added_lines(diff), parse_profile(text, module_path(repo)), line_text)
    for path, lines in result.uncovered.items():
        for rng in format_ranges(lines):
            print("UNCOVERED {}:{}".format(path, rng))
    for path in result.missing_files:
        print("NOT_IN_PROFILE {}（整个文件无覆盖数据；确认其不含可执行语句后才可人工豁免）".format(path))
    for path in result.non_go:
        print("MANUAL {}（无自动覆盖数据，须逐项映射到通过的测试）".format(path))
    uncovered = sum(len(v) for v in result.uncovered.values())
    print("diff-coverage: 检查 {} 行语句改动，未覆盖 {} 行，不在 profile 的文件 {} 个，需人工核对 {} 个".format(
        result.checked, uncovered, len(result.missing_files), len(result.non_go)))
    return 1 if uncovered or result.missing_files else 0


def cmd_docs(repo: Path, args: argparse.Namespace) -> int:
    base = resolve_base(repo, args.base)
    status = git(repo, "diff", "--name-status", "--no-renames", base + "...HEAD")
    entries = [tuple(line.split("\t", 1)) for line in status.splitlines() if "\t" in line]
    entries = [(s[0], p) for s, p in entries]
    doc = read_doc(repo, REGRESSION_DOC)
    findings = check_docs(entries, doc)
    diff = git(repo, "diff", "-U0", "--no-renames", "--diff-filter=AM", base + "...HEAD", "--", "*_test.go")
    for name in parse_added_test_funcs(diff):
        if name not in doc:
            print("HINT 新增测试函数回归文档未提及（确认所属段落已覆盖）: " + name)
    for finding in findings:
        print(finding)
    print("docs: 变更文件 {} 个，未满足 {} 项".format(len(entries), len(findings)))
    return 1 if findings else 0


def cmd_ledger(repo: Path, args: argparse.Namespace) -> int:
    path = repo / FAKE_LEDGER
    if not path.is_file():
        raise GateError("缺少 FAKE 缺陷台账: " + FAKE_LEDGER)
    errors, entries = lint_ledger(path.read_text(encoding="utf-8"))

    def exists(name: str) -> bool:
        proc = subprocess.run(["git", "-C", str(repo), "grep", "-q", "--untracked", "-F", "-e", "func " + name + "(", "--", "*.go"],
                              capture_output=True)
        return proc.returncode == 0

    errors += missing_cases(entries, exists)
    for error in errors:
        print("LEDGER " + error)
    blocking = open_entries(entries) if args.release else []
    for entry_id in blocking:
        print("LEDGER {} 状态为 open，发布前必须修复或改为 accepted 并写明理由".format(entry_id))
    print("ledger: {} 条，格式错误 {}，发布阻断 {}".format(len(entries), len(errors), len(blocking)))
    return 1 if errors or blocking else 0


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repo", default=".", help="仓库根目录")
    sub = parser.add_subparsers(dest="command", required=True)
    cov = sub.add_parser("diff-coverage")
    cov.add_argument("--base", required=True)
    cov.add_argument("--profile", required=True)
    docs = sub.add_parser("docs")
    docs.add_argument("--base", required=True)
    ledger = sub.add_parser("ledger")
    ledger.add_argument("--release", action="store_true")
    try:
        args = parser.parse_args(argv)
    except SystemExit as exc:
        return 2 if exc.code else 0
    repo = Path(args.repo).resolve()
    handlers = {"diff-coverage": cmd_diff_coverage, "docs": cmd_docs, "ledger": cmd_ledger}
    try:
        return handlers[args.command](repo, args)
    except GateError as exc:
        print("release-gate: ERROR: {}".format(exc), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
