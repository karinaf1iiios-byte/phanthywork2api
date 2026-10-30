#!/usr/bin/env bash
# credit.sh — 账号池积分日报（表格 + 汇总行）
#
# 用法:
#   ./scripts/credit.sh            # 人读表格
#   ./scripts/credit.sh --json     # 原始 JSON（喂给别的工具）
#   ./scripts/credit.sh --quiet    # 只有汇总行（适合 cron/告警）
#
# 数据源：cmd/activity -action status（每个号打一次 summary，顺带刷新 credits）
# 图表版看板在网关的 /ui/，这个脚本只做终端速览、不依赖网关在跑。
set -euo pipefail
cd "$(dirname "$0")/.."

JSON=0
QUIET=0
for a in "$@"; do
    case "$a" in
        --json) JSON=1 ;;
        --quiet|-q) QUIET=1 ;;
        -h|--help) sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "未知参数: $a" >&2; exit 1 ;;
    esac
done

command -v go >/dev/null || { echo "需要 go" >&2; exit 1; }

RAW=$(go run ./cmd/activity -action status -json 2>/dev/null || true)
if [[ -z "$RAW" ]]; then
    echo "没拿到状态（auths/ 下没有账号？或上游不通）" >&2
    exit 1
fi

if [[ $JSON -eq 1 ]]; then
    printf '%s\n' "$RAW"
    exit 0
fi

printf '%s' "$RAW" | CREDIT_QUIET="$QUIET" python3 -c '
import json, os, sys

rows = json.load(sys.stdin)
if not isinstance(rows, list):
    print("状态不是列表：", str(rows)[:200])
    sys.exit(1)

quiet = os.environ.get("CREDIT_QUIET") == "1"
total = 0
todo, failed, out = [], [], []

for r in rows:
    uid = (r.get("uid") or "")
    uid_short = uid  # 完整 uid（phanthy-<ts> 共 18 字符）比截断好认
    credits = r.get("creditsAvailable") or 0
    total += credits
    daily = r.get("dailyStatus") or "-"
    streak = r.get("streakDay") or 0
    earned = r.get("onboardingEarned") or 0
    omax = r.get("onboardingMax") or 0
    err = r.get("error") or ""
    if "temporarily_unavailable" in err:
        err = "活动服务临时不可用（503，不影响推理）"
    if err:
        failed.append("%s: %s" % (uid_short, err[:70]))
    if daily not in ("granted_today", "already_granted"):
        todo.append(uid_short)
    claim = "已签" if daily in ("granted_today", "already_granted") else daily
    out.append((uid_short, credits, claim, streak, "%d/%d" % (earned, omax), "!" if err else ""))

if not quiet:
    print("账号                积分   今日签到  连签  引导任务    备注")
    print("-" * 66)
    for uid, credits, claim, streak, ob, flag in out:
        print("%-18s %6s  %-9s %3s  %9s   %s" % (uid, format(credits, ","), claim, streak, ob, flag))
    print("-" * 66)

print("汇总：%d 个号 · 积分合计 %s · 未签到 %d · 异常 %d"
      % (len(rows), format(total, ","), len(todo), len(failed)))
for t in todo:
    print("  未签：%s" % t)
for f in failed:
    print("  异常：%s" % f)
'
