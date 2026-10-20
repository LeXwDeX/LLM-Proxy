#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${COMPOSE_FILE:-${ROOT_DIR}/docker-compose.yml}"
SERVICE_NAME="${SERVICE_NAME:-llms-proxy}"
NO_CACHE="${NO_CACHE:-1}"
TAIL_LINES="${TAIL_LINES:-120}"

if ! command -v docker >/dev/null 2>&1; then
  echo "[error] docker 未安装或不在 PATH 中" >&2
  exit 1
fi

if docker compose version >/dev/null 2>&1; then
  COMPOSE_CMD=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE_CMD=(docker-compose)
else
  echo "[error] 未找到 docker compose（插件或 docker-compose 命令）" >&2
  exit 1
fi

if [[ ! -f "${COMPOSE_FILE}" ]]; then
  echo "[error] compose 文件不存在: ${COMPOSE_FILE}" >&2
  exit 1
fi

echo "[info] 项目目录: ${ROOT_DIR}"
echo "[info] compose 文件: ${COMPOSE_FILE}"
echo "[info] 服务名: ${SERVICE_NAME}"

echo "[step] 修复挂载目录权限"
mkdir -p "${ROOT_DIR}/config" "${ROOT_DIR}/data" "${ROOT_DIR}/logs"
chmod 700 "${ROOT_DIR}/config" "${ROOT_DIR}/data" "${ROOT_DIR}/logs"
# config.json 仅属主可读写（Compose 以 user: "0:0" 运行容器）
[[ -f "${ROOT_DIR}/config/config.json" ]] && chmod 600 "${ROOT_DIR}/config/config.json"

echo "[step] 重新构建镜像"
BUILD_ARGS=()
if [[ "${NO_CACHE,,}" == "1" || "${NO_CACHE,,}" == "true" || "${NO_CACHE,,}" == "yes" ]]; then
  BUILD_ARGS+=(--no-cache)
fi
"${COMPOSE_CMD[@]}" -f "${COMPOSE_FILE}" build "${BUILD_ARGS[@]}" "${SERVICE_NAME}"

echo "[step] 重新启动服务"
"${COMPOSE_CMD[@]}" -f "${COMPOSE_FILE}" up -d --force-recreate --remove-orphans "${SERVICE_NAME}"

echo "[step] 查看容器状态"
"${COMPOSE_CMD[@]}" -f "${COMPOSE_FILE}" ps "${SERVICE_NAME}"

CONTAINER_ID="$("${COMPOSE_CMD[@]}" -f "${COMPOSE_FILE}" ps -q "${SERVICE_NAME}" || true)"
if [[ -n "${CONTAINER_ID}" ]]; then
  echo "[step] 最近 ${TAIL_LINES} 行日志"
  docker logs --tail "${TAIL_LINES}" "${CONTAINER_ID}"
else
  echo "[warn] 未找到运行中的容器，请检查 compose 输出" >&2
fi

echo "[done] 重建并重启完成"
