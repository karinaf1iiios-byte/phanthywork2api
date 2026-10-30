#!/usr/bin/env bash
# checkin.sh — 批量签到（薄包装：签到逻辑在 cmd/activity -action claim，脚本只做汇总）
#
# 用法:
#   ./scripts/checkin.sh           # 挨个号签一次，打印汇总
#   ./scripts/checkin.sh --dry     # 只看今天能不能签，不实际领
#   ./scripts/checkin.sh --json    # 原始 JSON
#
# 服务端幂等：同一天重复跑不会多领，也不会扣分，cron 里多打几次是安全的。
set -euo pipefail
cd "$(dirname "$0")/.."

DRY=0
JSON=0
for a in "$@"; do
    case "$a" in
        --dry) DRY=1 ;;
        --json) JSON=1 ;;
        -h|--help) sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "未知参数: $a" >&2; exit 1 ;;
    esac
done

command -v go >/dev/null || { echo "需要 go" >&2; exit 1; }

if [[ $DRY -eq 1 ]]; then
    echo "（--dry：只查询，不领取）"
    exec ./scripts/credit.sh "${@:2}"
fi

RAW=$(go run ./cmd/activity -action claim -json 2>/dev/null || true)
if [[ -z "$RAW" ]]; then
    echo "签到失败：没拿到返回（auths/ 空？上游不通？）" >&2
    exit 1
fi

if [[ $JSON -eq 1 ]]; then
    printf '%s\n' "$RAW"
    exit 0
fi

printf '%s' "$RAW" | python3 -c '
import json, sys

rows = json.load(sys.stdin)
if not isinstance(rows, list):
    print("返回不是列表：", str(rows)[:200]); sys.exit(1)

ok = already = fail = warn = 0
points = 0
for r in rows:
    uid = r.get("uid") or ""
    err = r.get("error") or ""
    status = r.get("dailyStatus") or "-"
    got = r.get("claimPoints") or 0
    streak = r.get("streakDay") or 0
    credits = r.get("creditsAvailable") or 0
    if "temporarily_unavailable" in err:
        # 上游活动服务偶发 503，和凭证无关；单独归类，不算失败
        warn += 1
        print("  ! %-18s 活动服务 503（稍后重试，不影响推理）" % uid)
        continue
    if err:
        fail += 1
        print("  ✗ %-18s %s" % (uid, err[:70]))
    elif status in ("granted", "granted_today"):
        ok += 1
        points += got
        print("  ✓ %-18s +%d 分  连签 %d 天  余额 %s" % (uid, got, streak, format(credits, ",")))
    else:
        already += 1
        print("  · %-18s 今日已领（%s） 余额 %s" % (uid, status, format(credits, ",")))

print("")
print("汇总：total=%d ok=%d already=%d warn=%d fail=%d 本次共 +%d 分" % (len(rows), ok, already, warn, fail, points))
sys.exit(1 if fail else 0)
'
