#!/usr/bin/env bash
# dashboard.sh — 起网关并打开积分看板
#
# 用法:
#   ./scripts/dashboard.sh            # 后台起服务（已在跑就复用）+ 打开浏览器
#   ./scripts/dashboard.sh --stop     # 停掉本脚本起的后台服务
#   ./scripts/dashboard.sh --no-open  # 只起服务，不开浏览器（服务器/远程用）
#   PORT=8080 ./scripts/dashboard.sh  # 换端口
#
# 后台服务的日志在 logs/dashboard.log，pid 在 data/dashboard.pid。
# 长期跑请用 ./scripts/install_schedule.sh --install（launchd 托管），别用这个脚本。
set -euo pipefail
cd "$(dirname "$0")/.."

PORT="${PORT:-7863}"
CONF="${CONF:-config.json}"
PID_FILE="data/dashboard.pid"
LOG="logs/dashboard.log"
BIN=".bin/p2a"

mkdir -p .bin data logs

running() {
    [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE" 2>/dev/null)" 2>/dev/null
}

if [[ "${1:-}" == "--stop" ]]; then
    if running; then
        kill "$(cat "$PID_FILE")" && rm -f "$PID_FILE"
        echo "已停止"
    else
        echo "没有在跑的后台实例"
    fi
    exit 0
fi

# 端口已被占用：可能是 launchd 托管的实例，直接复用
if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "端口 $PORT 已有服务在监听，直接打开看板"
else
    command -v go >/dev/null || { echo "需要 go" >&2; exit 1; }
    # 前端资源是 go:embed 进二进制的，改 app.css/app.js 也必须重编，
    # 所以新鲜度判断要把 internal/server/web/ 一起算上。
    if [[ ! -x "$BIN" ]] || find . \( -name '*.go' -o -path './internal/server/web/*' \) -newer "$BIN" -print -quit | grep -q .; then
        echo "编译网关…"
        go build -o "$BIN" ./cmd/server
    fi
    if [[ ! -f "$CONF" ]]; then
        echo "没有 ${CONF}，从 config.example.json 复制一份（记得改 api_key / claim_enabled）"
        cp config.example.json "$CONF"
    fi
    nohup "$BIN" -config "$CONF" -listen "127.0.0.1:$PORT" >>"${LOG}" 2>&1 &
    echo $! >"${PID_FILE}"
    # 最多等 15 秒：进程还活着就再试一次健康检查（刚 bind 上的瞬间 curl 会失败）
    ok=0
    for _ in $(seq 1 50); do
        sleep 0.3
        if curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then ok=1; break; fi
        if ! running; then break; fi
    done
    if [[ $ok -eq 0 ]]; then
        echo "起不来，看 ${LOG}：" >&2
        tail -20 "${LOG}" >&2
        rm -f "${PID_FILE}"
        exit 1
    fi
    echo "已启动（pid $(cat "$PID_FILE")，日志 ${LOG}）"
fi

URL="http://127.0.0.1:$PORT/ui/"
echo ""
echo "看板      $URL"
echo "登录页    http://127.0.0.1:$PORT/ui/login.html"
echo "OpenAI 兼容  http://127.0.0.1:$PORT/v1/chat/completions"
echo ""
if [[ "${1:-}" != "--no-open" ]] && command -v open >/dev/null; then
    open "$URL" 2>/dev/null || true
fi
