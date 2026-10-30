#!/usr/bin/env bash
# acct.sh — 账号运维（列出 / 详情 / 换机 / 移除）
#
# 用法:
#   ./scripts/acct.sh list                      # 账号 + 装机身份 + 凭证剩余寿命
#   ./scripts/acct.sh show <uid 或 scope>       # 单号详情（不打上游）
#   ./scripts/acct.sh rotate <uid 或 scope>     # 换装机身份（旧身份自动备份）
#   ./scripts/acct.sh rm <uid> [--yes]          # 移除账号（先备份到 auths/.trash/）
#   ./scripts/acct.sh path                      # 打印 auths/ 的绝对路径
#
# 注意：rotate = 换机，不是续期。三把 seed 一起换，服务端会当成新装机，
#       正在跑的任务的粘性会话会失效——除非你确实要换机器，否则别动。
set -euo pipefail
cd "$(dirname "$0")/.."
AUTH_DIR="./auths"

usage() { sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

cmd="${1:-list}"; shift || true

case "$cmd" in
    path)
        echo "$(cd "$AUTH_DIR" && pwd)"
        ;;

    list)
        command -v python3 >/dev/null || { echo "需要 python3" >&2; exit 1; }
        AUTH_DIR="$AUTH_DIR" python3 - <<'PY'
import base64, glob, json, os, time


def jwt_sub(tok):
    """access token 的 sub 就是 accountScope（装机身份文件名用它）。不验签，只读 claim。"""
    try:
        seg = tok.split(".")[1]
        seg += "=" * (-len(seg) % 4)
        return json.loads(base64.urlsafe_b64decode(seg)).get("sub") or ""
    except Exception:
        return ""


d = os.environ["AUTH_DIR"]
accs = sorted(glob.glob(os.path.join(d, "phanthy-*.json")))
insts = sorted(glob.glob(os.path.join(d, "installation-*.json")))
if not accs:
    print("auths/ 下还没有账号，用 ./scripts/login.sh 登录一个")
    raise SystemExit(0)

print("账号                凭证剩余   装机身份  scope")
print("-" * 66)
now = time.time()
for p in accs:
    try:
        doc = json.load(open(p))
    except Exception as e:
        print("%-18s 读取失败: %s" % (os.path.basename(p), e))
        continue
    a = doc.get("auth", doc)
    exp = a.get("expiresAt") or 0
    left = int(exp - now) if exp else None
    left_txt = ("%d 分钟" % (left // 60)) if left and left > 0 else ("已过期" if exp else "未知")
    scope = doc.get("accountScope") or jwt_sub(a.get("accessToken") or "")
    # 身份文件名就是 installation-<scope>.json
    has_inst = "有" if scope and os.path.exists(os.path.join(d, "installation-%s.json" % scope)) else "缺"
    print("%-18s %-9s %-8s %s" % (os.path.basename(p)[:-5], left_txt, has_inst, scope or "-"))

print("")
print("共 %d 个账号凭证、%d 个装机身份" % (len(accs), len(insts)))
print("提示：装机身份缺了会自动重建（服务端当新装机），换机器前先备份 auths/")
PY
        ;;

    show)
        key="${1:-}"; [[ -n "$key" ]] || { echo "用法: ./scripts/acct.sh show <uid 或 scope>" >&2; exit 1; }
        AUTH_DIR="$AUTH_DIR" KEY="$key" python3 - <<'PY'
import base64, glob, json, os


def jwt_sub(tok):
    try:
        seg = tok.split(".")[1]
        seg += "=" * (-len(seg) % 4)
        return json.loads(base64.urlsafe_b64decode(seg)).get("sub") or ""
    except Exception:
        return ""


d, key = os.environ["AUTH_DIR"], os.environ["KEY"]
hit = None
for p in glob.glob(os.path.join(d, "phanthy-*.json")):
    doc = json.load(open(p))
    a = doc.get("auth", doc)
    scope = doc.get("accountScope") or jwt_sub(a.get("accessToken") or "")
    if key in (scope, os.path.basename(p)[:-5], (doc.get("account") or {}).get("uid")):
        hit = (p, doc, a)
        break
if not hit:
    print("没找到：%s" % key); raise SystemExit(1)
p, doc, a = hit
acct = doc.get("account") or {}
print("文件      %s" % p)
print("uid       %s" % (acct.get("uid") or "-"))
print("昵称      %s" % (acct.get("nickname") or "-"))
scope = doc.get("accountScope") or jwt_sub(a.get("accessToken") or "")
print("scope     %s" % (scope or "-"))
print("expiresAt %s" % a.get("expiresAt"))
print("refresh   %s" % ("有" if a.get("refreshToken") else "无（过期后要重新登录）"))
print("idToken   %s" % ("有" if a.get("idToken") else "无"))
ident = os.path.join(d, "installation-%s.json" % scope)
print("装机身份  %s" % (ident if os.path.exists(ident) else "缺（下次运行会自动重建）"))
PY
        ;;

    rotate)
        key="${1:-}"; [[ -n "$key" ]] || { echo "用法: ./scripts/acct.sh rotate <uid 或 scope>" >&2; exit 1; }
        echo "换装机会让服务端把这个号当成新机器，粘性会话全部失效。"
        printf '确认？(y/N) '
        read -r ans
        [[ "$ans" == "y" || "$ans" == "Y" ]] || { echo "已取消"; exit 1; }
        exec go run ./cmd/activity -action rotate -account "$key"
        ;;

    rm)
        key="${1:-}"; yes=0
        [[ "${2:-}" == "--yes" ]] && yes=1
        [[ -n "$key" ]] || { echo "用法: ./scripts/acct.sh rm <uid> [--yes]" >&2; exit 1; }
        f="$AUTH_DIR/$key.json"
        [[ -f "$f" ]] || { echo "没有这个文件：$f" >&2; exit 1; }
        if [[ $yes -eq 0 ]]; then
            printf '把 %s 移出池子（备份到 auths/.trash/）？(y/N) ' "$f"
            read -r ans
            [[ "$ans" == "y" || "$ans" == "Y" ]] || { echo "已取消"; exit 1; }
        fi
        mkdir -p "$AUTH_DIR/.trash"
        mv "$f" "$AUTH_DIR/.trash/$(basename "$f").$(date +%Y%m%d-%H%M%S)"
        echo "已移出：${f}（备份在 $AUTH_DIR/.trash/）"
        ;;

    -h|--help|help) usage 0 ;;
    *) echo "未知子命令: $cmd" >&2; usage 1 ;;
esac
