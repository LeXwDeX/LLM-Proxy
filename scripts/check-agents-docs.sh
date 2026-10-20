#!/usr/bin/env bash
# 校验共享 Agent 指引存在。
#
# 契约：AGENTS.md 是共享 Agent 指引的唯一规范源。
#
# 用法：bash scripts/check-agents-docs.sh   （或 make check-docs，已挂入 make test）

set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [ ! -f "$ROOT/AGENTS.md" ]; then
    echo "check-agents-docs: ERROR: AGENTS.md 不存在（规范源缺失）" >&2
    exit 1
fi

echo "check-agents-docs: OK（AGENTS.md 存在）"
