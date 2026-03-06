#!/usr/bin/env bash
# CLIProxyAPIPlus 本地开发管理脚本
# 用法: cliproxy <command>

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

PROJECT_DIR="${CLIPROXY_PROJECT_DIR:-${DEFAULT_PROJECT_DIR}}"
CONFIG_DIR="${CLIPROXY_CONFIG_DIR:-${PROJECT_DIR}}"
LOG_FILE="${CLIPROXY_LOG_FILE:-/tmp/cli-proxy.log}"
PID_FILE="${CLIPROXY_PID_FILE:-/tmp/cli-proxy.pid}"
DEV_BIN="${CLIPROXY_DEV_BIN:-/tmp/cli-proxy-dev}"
API_BASE_URL="${CLIPROXY_API_BASE_URL:-http://localhost:8317}"
API_KEY="${CLIPROXY_API_KEY:-}"

RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m'

api_get() {
    local path="$1"
    if [ -n "$API_KEY" ]; then
        curl -s -H "Authorization: Bearer ${API_KEY}" "${API_BASE_URL}${path}"
    else
        curl -s "${API_BASE_URL}${path}"
    fi
}

get_pid() {
    if [ -f "$PID_FILE" ]; then
        local pid
        pid="$(cat "$PID_FILE" 2>/dev/null || true)"
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            echo "$pid"
            return 0
        fi
    fi

    local pid
    pid="$(pgrep -f "$DEV_BIN" 2>/dev/null | head -1 || true)"
    if [ -n "$pid" ]; then
        echo "$pid"
        return 0
    fi

    pid="$(pgrep -f "cli-proxy-bin" 2>/dev/null | head -1 || true)"
    if [ -n "$pid" ]; then
        echo "$pid"
        return 0
    fi

    return 1
}

do_build() {
    if [ ! -d "$PROJECT_DIR" ]; then
        echo -e "${RED}项目目录不存在: ${PROJECT_DIR}${NC}"
        return 1
    fi

    echo -e "${BLUE}编译中...${NC}"
    local commit
    local build_date
    commit="$(git -C "$PROJECT_DIR" rev-parse --short HEAD 2>/dev/null || echo "none")"
    build_date="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"

    if ! (
        cd "$PROJECT_DIR" && \
        go build -o "$DEV_BIN" \
            -ldflags "-X 'main.Version=dev' -X 'main.Commit=${commit}' -X 'main.BuildDate=${build_date}'" \
            ./cmd/server/
    ); then
        echo -e "${RED}编译失败${NC}"
        return 1
    fi
    echo -e "${GREEN}编译完成${NC}"
    return 0
}

show_help() {
    cat <<EOF
cli-proxy 本地开发管理脚本

用法: cliproxy <命令>

命令:
  start    编译并启动服务
  stop     停止服务
  restart  重新编译并重启
  status   查看状态
  log      实时查看日志
  err      查看错误日志
  models   列出可用模型
  build    仅编译不启动
  version  查看版本信息
  update   git pull 并重启

环境变量:
  CLIPROXY_PROJECT_DIR   项目目录 (默认: ${PROJECT_DIR})
  CLIPROXY_CONFIG_DIR    运行目录/配置目录 (默认: ${CONFIG_DIR})
  CLIPROXY_API_BASE_URL  API 地址 (默认: ${API_BASE_URL})
  CLIPROXY_API_KEY       API Key (默认: 空)
  CLIPROXY_LOG_FILE      日志文件 (默认: ${LOG_FILE})
  CLIPROXY_PID_FILE      PID 文件 (默认: ${PID_FILE})
  CLIPROXY_DEV_BIN       编译产物路径 (默认: ${DEV_BIN})
EOF
}

cmd="${1:-}"
case "$cmd" in
    start)
        PID="$(get_pid || true)"
        if [ -n "${PID}" ]; then
            echo -e "${BLUE}cli-proxy 已在运行 (PID: ${PID})${NC}"
            exit 0
        fi
        do_build || exit 1
        if [ ! -d "$CONFIG_DIR" ]; then
            echo -e "${RED}运行目录不存在: ${CONFIG_DIR}${NC}"
            exit 1
        fi
        echo -e "${BLUE}启动 cli-proxy...${NC}"
        cd "$CONFIG_DIR"
        nohup "$DEV_BIN" >> "$LOG_FILE" 2>&1 &
        echo $! > "$PID_FILE"
        sleep 1
        PID="$(get_pid || true)"
        if [ -n "${PID}" ]; then
            echo -e "${GREEN}已启动 (PID: ${PID})${NC}"
            echo -e "${BLUE}日志: ${LOG_FILE}${NC}"
        else
            echo -e "${RED}启动失败，查看日志: tail -f ${LOG_FILE}${NC}"
            exit 1
        fi
        ;;
    stop)
        PID="$(get_pid || true)"
        if [ -z "${PID}" ]; then
            echo -e "${BLUE}cli-proxy 未运行${NC}"
            exit 0
        fi
        echo -e "${BLUE}停止 cli-proxy (PID: ${PID})...${NC}"
        kill "$PID" 2>/dev/null || true
        sleep 1
        if [ -z "$(get_pid || true)" ]; then
            echo -e "${GREEN}已停止${NC}"
            rm -f "$PID_FILE"
        else
            echo -e "${RED}停止失败，尝试强制终止...${NC}"
            kill -9 "$PID" 2>/dev/null || true
        fi
        ;;
    restart)
        "$0" stop
        sleep 1
        "$0" start
        ;;
    status)
        PID="$(get_pid || true)"
        if [ -n "${PID}" ]; then
            echo -e "${GREEN}cli-proxy 运行中 (PID: ${PID})${NC}"
            echo
            local_models="$(api_get "/v1/models" | jq -r '.data | length' 2>/dev/null || true)"
            if [ -n "$local_models" ] && [ "$local_models" != "null" ]; then
                echo -e "${BLUE}可用模型数: ${local_models}${NC}"
            fi
            echo -e "${BLUE}运行目录: ${CONFIG_DIR}${NC}"
            echo -e "${BLUE}项目目录: ${PROJECT_DIR}${NC}"
            echo -e "${BLUE}日志文件: ${LOG_FILE}${NC}"
            echo -e "${BLUE}API 地址: ${API_BASE_URL}${NC}"
        else
            echo -e "${RED}cli-proxy 未运行${NC}"
        fi
        ;;
    log)
        tail -f "$LOG_FILE"
        ;;
    err)
        grep -i "error\|fail\|warn" "$LOG_FILE" | tail -50
        ;;
    models)
        api_get "/v1/models" | jq -r '.data[].id' 2>/dev/null | sort
        ;;
    build)
        do_build
        ;;
    version)
        if [ -x "$DEV_BIN" ]; then
            "$DEV_BIN" --help 2>&1 | head -1
        else
            echo "dev binary not built yet, run: cliproxy build"
        fi
        commit="$(git -C "$PROJECT_DIR" rev-parse --short HEAD 2>/dev/null || true)"
        branch="$(git -C "$PROJECT_DIR" branch --show-current 2>/dev/null || true)"
        if [ -n "$commit" ]; then
            echo -e "${BLUE}Git: ${branch} @ ${commit}${NC}"
        fi
        ;;
    update)
        echo -e "${BLUE}拉取最新代码...${NC}"
        if ! git -C "$PROJECT_DIR" pull; then
            echo -e "${RED}git pull 失败${NC}"
            exit 1
        fi
        echo -e "${GREEN}代码已更新${NC}"
        PID="$(get_pid || true)"
        if [ -n "${PID}" ]; then
            echo -e "${BLUE}重新编译并重启...${NC}"
            "$0" restart
        fi
        ;;
    *)
        show_help
        ;;
esac
