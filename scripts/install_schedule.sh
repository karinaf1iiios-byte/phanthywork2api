#!/bin/bash
# 把账号池调度装进 launchd（macOS 原生定时）。默认只打印，不落盘、不加载。
#
#   ./scripts/install_schedule.sh            # dry-run：打印全部日历任务
#   ./scripts/install_schedule.sh --install   # 写入 ~/Library/LaunchAgents 并加载
#   ./scripts/install_schedule.sh --status    # 看是否在跑
#   ./scripts/install_schedule.sh --uninstall # 卸载并删除
#
# 时间表（本地时区，互不重叠；互斥锁在 pool.sh 里，日历只负责叫醒）：
#   08:50 invite  09:00 claim  09:30 verify  09:40 sweep
#   20:50 invite  21:00 claim  21:30 verify  21:40 sweep
# invite = 产出「带码注册」链接（邀请关系只在注册表单建立，事后补填不可能）；
# sweep  = 补 onboarding（scripts/sweep.py，只对还没满 1500 的号动手）。
# 注意：bind / bindwatch 已删除 —— `POST /referrals/bind` 对已创建账号永久 409。
# 两趟签到是因为要「不漏」：接口幂等，第二趟如果是同一自然日只会返回 already_granted。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AGENTS="$HOME/Library/LaunchAgents"
PREFIX="com.phanthywork.pool"
JOBS=(
  "invite8:invite:8:50"
  "claim9:claim:9:0"
  "verify9:verify:9:30"
  "sweep9:sweep --apply:9:40"
  "invite20:invite:20:50"
  "claim21:claim:21:0"
  "verify21:verify:21:30"
  "sweep21:sweep --apply:21:40"
)

plist() { # name cmd hour minute —— cmd 里可以带空格（如 "sweep --apply"）
  local cmdline="" a
  for a in $2; do cmdline="$cmdline    <string>$a</string>
"; done
  cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$PREFIX.$1</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>$ROOT/scripts/pool.sh</string>
$cmdline
  </array>
  <key>WorkingDirectory</key><string>$ROOT</string>
  <key>StartCalendarInterval</key>
  <dict><key>Hour</key><integer>$3</integer><key>Minute</key><integer>$4</integer></dict>
  <key>StandardOutPath</key><string>$ROOT/logs/launchd-$1.log</string>
  <key>StandardErrorPath</key><string>$ROOT/logs/launchd-$1.err.log</string>
  <key>RunAtLoad</key><false/>
  <key>ProcessType</key><string>Background</string>
</dict>
</plist>
EOF
}

case "${1:-}" in
  --install)
    mkdir -p "$AGENTS" "$ROOT/logs"
    for spec in "${JOBS[@]}"; do
      IFS=: read -r name cmd hour min <<<"$spec"
      target="$AGENTS/$PREFIX.$name.plist"
      plist "$name" "$cmd" "$hour" "$min" > "$target"
      launchctl bootout "gui/$(id -u)/$PREFIX.$name" 2>/dev/null
      launchctl bootstrap "gui/$(id -u)" "$target" || exit 1
      echo "已加载 $PREFIX.$name  ${hour}:$(printf '%02d' "$min")  → pool.sh $cmd"
    done
    chmod +x "$ROOT/scripts/pool.sh"
    ;;
  --uninstall)
    # 历史遗留的 bindwatch（已废弃）
    launchctl bootout "gui/$(id -u)/$PREFIX.bindwatch" 2>/dev/null
    rm -f "$AGENTS/$PREFIX.bindwatch.plist"
    for spec in "${JOBS[@]}"; do
      IFS=: read -r name _ _ _ <<<"$spec"
      launchctl bootout "gui/$(id -u)/$PREFIX.$name" 2>/dev/null
      rm -f "$AGENTS/$PREFIX.$name.plist"
      echo "已卸载 $PREFIX.$name"
    done
    ;;
  --status)
    for spec in "${JOBS[@]}"; do
      IFS=: read -r name _ _ _ <<<"$spec"
      if launchctl print "gui/$(id -u)/$PREFIX.$name" >/dev/null 2>&1; then
        echo "loaded   $PREFIX.$name"
      else
        echo "missing  $PREFIX.$name"
      fi
    done
    ;;
  ""|--dry-run)
    echo "# 干跑：不会写入任何文件。加 --install 才真的装。"
    for spec in "${JOBS[@]}"; do
      IFS=: read -r name cmd hour min <<<"$spec"
      echo "### $PREFIX.$name  ${hour}:$(printf '%02d' "$min")  → $ROOT/scripts/pool.sh $cmd"
      plist "$name" "$cmd" "$hour" "$min"
    done
    ;;
  *)
    echo "用法: install_schedule.sh [--dry-run|--install|--status|--uninstall]"; exit 2
    ;;
esac
