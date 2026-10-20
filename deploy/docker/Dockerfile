#
# 构建时建议禁用 BuildKit 缓存，并传入源提交 SHA（写入 OCI revision 标签）：
#   docker build --no-cache --build-arg LLMS_SOURCE_COMMIT=$(git rev-parse HEAD) \
#     -t llms-proxy:latest -f deploy/docker/Dockerfile .
#

ARG GO_VERSION=1.27.2
# 构建序号：CI / Makefile 通过 --build-arg BUILD_NUMBER=... 注入；
# 未注入时回退到 git 提交计数，再不行为 0。版本号格式 YYYYMMDD.BUILD。
ARG BUILD_NUMBER=0

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /workspace
ARG BUILD_NUMBER

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Auto-update model catalog (best-effort: failure keeps existing data)
RUN pip install --no-cache-dir --break-system-packages requests 2>/dev/null; \
    if curl -sSf --connect-timeout 10 --max-time 30 https://models.dev/api.json -o /tmp/models_dev_raw.json 2>/dev/null; then \
      python3 scripts/update-model-catalog.py /tmp/models_dev_raw.json internal/catalog/data/models.json \
      && echo ">> catalog updated" \
      || echo ">> catalog transform failed, keeping existing"; \
      rm -f /tmp/models_dev_raw.json; \
    else \
      echo ">> catalog download failed, keeping existing data"; \
    fi

RUN --mount=type=cache,target=/go/pkg/mod \
    VERSION="$(date +%Y%m%d).${BUILD_NUMBER:-$(git rev-list --count HEAD 2>/dev/null || echo 0)}" && \
    echo ">> building version ${VERSION}" && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags "-X github.com/LeXwDeX/LLM-Proxy/internal/version.Version=${VERSION}" -o /workspace/bin/llms-proxy ./cmd/proxy

FROM alpine:3.19
ARG LLMS_SOURCE_COMMIT
LABEL org.opencontainers.image.title="LLMs Proxy" \
      org.opencontainers.image.description="Lightweight proxy with auth, logging, and failover for LLMs Endpoints" \
      org.opencontainers.image.source="https://github.com/LeXwDeX/LLM-Proxy" \
      org.opencontainers.image.revision=${LLMS_SOURCE_COMMIT}

RUN mkdir -p /etc/llms-proxy /var/log/llms-proxy /var/lib/llms-proxy

WORKDIR /app
COPY --from=build /workspace/bin/llms-proxy /usr/local/bin/llms-proxy

EXPOSE 8080
VOLUME ["/etc/llms-proxy", "/var/log/llms-proxy", "/var/lib/llms-proxy"]

CMD ["/usr/local/bin/llms-proxy", "-config", "/etc/llms-proxy/config.json"]
