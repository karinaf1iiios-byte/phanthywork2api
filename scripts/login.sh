#!/usr/bin/env bash
# login.sh — Phanthy 账号登录（OAuth 授权码 + PKCE）→ 落盘 auths/phanthy-*.json
#
# 用法:
#   ./scripts/login.sh                 # 交互式：生成链接 → 浏览器授权 → 粘授权码
#   ./scripts/login.sh --code=XXXX     # 非交互：直接给授权码（要先跑过一次生成链接）
#   ./scripts/login.sh --url           # 只生成链接，不改任何凭证
#   ./scripts/login.sh --ui            # 打开可视化登录页（需要网关在跑）
#
# 说明:
#   * PKCE verifier 存在 .login-verifier，只能用一次；重启流程就重新生成。
#   * 授权码一次性、几分钟内有效；报 invalid_grant 就重跑一遍。
#   * 落盘文件权限 0600；配合 `./scripts/credit.sh` 看新号的积分。
set -euo pipefail

cd "$(dirname "$0")/.."
AUTH_DIR="./auths"
VERIFIER=".login-verifier"
BIN="./.bin/login"

usage() { sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

CODE=""
URL_ONLY=0
OPEN_UI=0
for arg in "$@"; do
    case "$arg" in
        --code=*) CODE="${arg#--code=}" ;;
        --url)    URL_ONLY=1 ;;
        --ui)     OPEN_UI=1 ;;
        -h|--help) usage 0 ;;
        *) echo "未知参数: $arg" >&2; usage 1 ;;
    esac
done

if [[ $OPEN_UI -eq 1 ]]; then
    PORT="${PORT:-7863}"
    URL="http://127.0.0.1:${PORT}/ui/login.html"
    echo "可视化登录页：$URL"
    echo "（需要网关已在跑：./scripts/dashboard.sh 或 ./p2a -config config.json）"
    command -v open >/dev/null && open "$URL" 2>/dev/null || true
    exit 0
fi

# ── 构建 login 二进制：源码或 go.mod 比它新就重编 ─────────────────────
need_build=0
if [[ ! -x "$BIN" ]]; then
    need_build=1
elif find . -name '*.go' -o -name 'go.mod' -o -name 'go.sum' | while read -r f; do
        [[ "$f" -nt "$BIN" ]] && { echo x; break; }
     done | grep -q x; then
    need_build=1
fi
if [[ $need_build -eq 1 ]]; then
    command -v go >/dev/null || { echo "需要 go 来构建 login（或把已编译的 login 放到 ${BIN}）" >&2; exit 1; }
    mkdir -p .bin
    echo "编译 login…"
    go build -o "$BIN" ./cmd/login
fi

mkdir -p "$AUTH_DIR"
if [[ ! -w "$AUTH_DIR" ]]; then
    echo "❌ $AUTH_DIR 不可写（被 chown 给别的 uid 了？）" >&2
    exit 1
fi

echo "============================================================"
echo "  Phanthy 登录（OAuth 授权码 + PKCE）"
echo "============================================================"

# ── 1) 生成授权链接 ────────────────────────────────────────────────
"$BIN" -step=url | tee /tmp/.phanthy-login-url.$$ >/dev/null
AUTH_URL=$(grep -oE 'https://[^"[:space:]]+' /tmp/.phanthy-login-url.$$ | head -1 || true)
rm -f /tmp/.phanthy-login-url.$$
if [[ -z "${AUTH_URL:-}" ]]; then
    echo "没拿到授权链接，检查 $BIN -step=url 的输出" >&2
    exit 1
fi

echo ""
echo "① 浏览器里打开下面链接，用要入池的那个账号登录："
echo ""
echo "   $AUTH_URL"
echo ""
if command -v pbcopy >/dev/null; then
    printf '%s' "$AUTH_URL" | pbcopy 2>/dev/null && echo "   （已复制到剪贴板）"
    command -v open >/dev/null && open "$AUTH_URL" 2>/dev/null && echo "   （已尝试用默认浏览器打开）"
fi
echo ""
echo "② 登录成功后页面会跳到 /oauth/code/success 并显示授权码。"

if [[ $URL_ONLY -eq 1 ]]; then
    echo ""
    echo "（--url 模式：verifier 已存在 ${VERIFIER}，拿到码后跑 ./scripts/login.sh --code=<码>）"
    exit 0
fi

if [[ -z "$CODE" ]]; then
    if [[ -t 0 ]]; then
        printf '\n③ 把授权码粘到这里回车（整段 URL 也行）： '
        read -r CODE
    else
        echo "非交互环境需要 --code=<授权码>" >&2
        exit 1
    fi
fi

if [[ -z "$CODE" ]]; then
    echo "授权码为空，退出" >&2
    exit 1
fi

echo ""
echo "正在交换 token…"
if ! "$BIN" -step=exchange -code="$CODE"; then
    echo ""
    echo "交换失败。常见原因：" >&2
    echo "  - 授权码用过一次了（一次性），重新跑 ./scripts/login.sh" >&2
    echo "  - 授权码过期（几分钟），同上" >&2
    echo "  - 复制时带了多余字符" >&2
    exit 1
fi

echo ""
echo "账号已入池。当前池子状态："
echo ""
if [[ -x "$BIN" ]]; then
    go run ./cmd/activity -action status 2>/dev/null || true
fi

PORT="${PORT:-7863}"
echo ""
echo "看板：http://127.0.0.1:${PORT}/ui/    （网关没跑就先 ./scripts/dashboard.sh）"
