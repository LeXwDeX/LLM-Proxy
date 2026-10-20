## 说明 / Summary

<!-- 改了什么、为什么。What changed and why. -->

关联 Issue / Linked issues: Closes #

## TDD 证据 / TDD evidence

<!--
行为变更：列出先失败的测试（命令与失败输出摘要），以及修复后同一测试通过。
保持行为不变的重构或仅文档改动：写明不适用的原因。
Behavior change: the test that failed first (command and failure excerpt) and the same test passing after the fix.
Behavior-preserving refactor or docs-only change: say why this does not apply.
-->

- 先失败 / Failing first:
- 修复后通过 / Passing after fix:

## 已跑门禁 / Gates run

<!-- 勾选本地实际执行过的检查；未执行的不要勾选。Tick only what you actually ran locally. -->

- [ ] `make test`
- [ ] `go vet ./...` + `go vet -tags=e2e ./test/e2e/`
- [ ] `go build ./...`
- [ ] race：`go test -race -count=1 ./internal/proxy/... ./internal/nosql/... ./internal/usage/... ./internal/quota/... ./internal/admin/... ./cmd/proxy/...`
- [ ] `make e2e`
- [ ] `make integration`
- [ ] `node scripts/testing/admin_ui_test.cjs`
- [ ] Python 脚本单测 / Python script tests
- [ ] `git diff --check`

## 未验证范围 / Not verified

<!-- 没有运行或无法在本地验证的部分（如真实上游、部署环境）。Anything not run or not verifiable locally (e.g. real upstreams, deployments). -->

## 影响 / Impact

- 计费或额度 / Billing or quota: 否 / No
- 协议、路径或用量解析 / Protocols, paths or usage parsing: 否 / No
- 兼容性（API、配置、存储格式）/ Compatibility (API, config, storage format): 否 / No

<!-- 如有影响，说明影响面、迁移与验证方式。If any is affected, describe the scope, migration and verification. -->
