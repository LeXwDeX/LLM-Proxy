BIN_DIR := bin
BINARY := $(BIN_DIR)/llms-proxy
CATALOG_RAW := /tmp/models_dev_raw.json
CATALOG_OUT := internal/catalog/data/models.json

# ===== 动态版本号 YYYYMMDD.BUILD =====
# 日期部分取构建当天；BUILD 部分默认取 git 提交计数，
# 可通过 `BUILD_NUMBER=... make build` 或环境变量覆盖（CI 一般注入 run_number）。
BUILD_DATE  := $(shell date +%Y%m%d)
BUILD_NUMBER ?= $(shell git rev-list --count HEAD 2>/dev/null || echo 0)
VERSION     := $(BUILD_DATE).$(BUILD_NUMBER)
VERSION_PKG := github.com/LeXwDeX/LLM-Proxy/internal/version
LDFLAGS     := -X $(VERSION_PKG).Version=$(VERSION)

.PHONY: all build test test-proxy test-fast-integrity coverage-check integration e2e ui-sim clean catalog docker version check-docs

all: test build

# 打印将要注入的版本号
version:
	@echo "$(VERSION)"

# ⚠️ Docker 构建必须 --no-cache，否则 BuildKit 缓存旧二进制
docker:
	@echo ">> building docker image (--no-cache)"
	docker build --no-cache --build-arg BUILD_NUMBER=$(BUILD_NUMBER) -t llms-proxy:latest -f deploy/docker/Dockerfile .

catalog:
	@echo ">> updating model catalog from models.dev"
	@if curl -sSf --connect-timeout 10 --max-time 30 https://models.dev/api.json -o $(CATALOG_RAW) 2>/dev/null; then \
		python3 scripts/update-model-catalog.py $(CATALOG_RAW) $(CATALOG_OUT) && \
		echo ">> catalog updated successfully"; \
	else \
		echo ">> catalog download failed, keeping existing data"; \
	fi

build: catalog
	@echo ">> building $(BINARY) (version $(VERSION))"
	@mkdir -p $(BIN_DIR)
	GO111MODULE=on go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/proxy

test: check-docs
	@echo ">> running unit tests"
	go test ./...

# 核心代理链路快速回归（高 churn 路径；internal/proxy 改动后的最小验证，完整验证用 make test）
test-proxy:
	@echo ">> running core proxy regression tests"
	go test ./internal/proxy/...

# Issue #125：用量完整性快速路径双模式。forcefastintegrity 标签只在测试构建中使用，
# 让每次完整性读取都在同一读事务内先分类再走窗口化快速路径，nosql 与 quota 全部
# 现有用例因此同时检验 Fast == Full。生产构建不带该标签。
test-fast-integrity:
	@echo ">> running nosql/quota tests with the forced fast integrity path"
	go test -tags forcefastintegrity ./internal/nosql/... ./internal/quota/...

# Issue #136：internal 覆盖率门禁。总覆盖率与关键包均不得低于 80%；
# 脚本按语句数汇总 cover profile，profile 缺失指定包或格式损坏时失败。
# 串行跑包（-p 1）：包间并行负载会让 internal/nosql 的 wall-clock 比值用例
# （TestRecordBatchTimeDoesNotScaleWithRetainedEventsOnUpgradedLedger）偶发超阈值。
# 不要改用 -race 规避，也不要删除或放宽该用例。
coverage-check:
	@echo ">> running Go coverage gate"
	go test -p 1 -count=1 -coverprofile=coverage.out ./internal/...
	python3 scripts/testing/check_go_coverage.py --profile coverage.out \
		--min-total 80 \
		--min-package internal/admin=80 \
		--min-package internal/proxy=80 \
		--min-package internal/nosql=80 \
		--min-package internal/usage=80 \
		--min-package internal/quota=80 \
		--min-package internal/pricing=80

# 校验共享 Agent 指引存在
check-docs:
	@bash scripts/check-agents-docs.sh

integration:
	@echo ">> running integration tests"
	go test -tags=integration ./test/...

# 虚假上游端到端：编译真实代理二进制，对接进程内 internal/fakellm，覆盖原厂 + Patch
# 厂商协议、WebSocket、负载切换与计费回读；不调用任何真实模型。-v 输出已知缺口清单。
e2e:
	@echo ">> running fake-upstream end-to-end tests"
	go test -tags=e2e -count=1 -v ./test/e2e/

# 管理后台网页全流程模拟：真实代理 + fakellm + 本机 Chromium（CDP，零依赖，不进 CI）。
ui-sim:
	@echo ">> running admin UI simulation"
	node scripts/testing/admin_ui_sim.cjs

clean:
	@echo ">> cleaning build artifacts"
	rm -rf $(BIN_DIR)
