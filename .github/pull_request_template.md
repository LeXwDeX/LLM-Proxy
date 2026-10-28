## 原因

<!-- 说明为什么需要此变更。 -->

## 变更

<!-- 说明改了什么，以及变更范围。 -->

<!-- 另起一行填写关联 Issue；需要自动关闭时，将 Closes 和实际 Issue 编号写在同一行。 -->

## 证据

### TDD 证据

<!--
行为变更：列出先失败的测试（命令与失败输出摘要），以及修复后同一测试通过的证据。
保持行为不变的重构或仅文档改动：写明不适用的原因。
-->

- 先失败：
- 修复后通过：

### 未验证范围

<!-- 说明没有运行或无法在本地验证的部分，例如真实上游和部署环境。 -->

### 影响

- 计费或额度：否
- 协议、路径或用量解析：否
- 兼容性（API、配置、存储格式）：否

<!-- 如有影响，说明影响面、迁移与验证方式。 -->

## 检查清单

<!-- 只勾选本地实际执行过的检查。 -->

- [ ] `make test`
- [ ] `go vet ./...` + `go vet -tags=e2e ./test/e2e/`
- [ ] `go build ./...`
- [ ] race：`go test -race -count=1 ./internal/proxy/... ./internal/nosql/... ./internal/usage/... ./internal/quota/... ./internal/admin/... ./cmd/proxy/...`
- [ ] `make e2e`
- [ ] `make integration`
- [ ] `node scripts/testing/admin_ui_test.cjs`
- [ ] `(cd internal/admin/ui && node --test)`
- [ ] Python 脚本单测
- [ ] `git diff --check`
