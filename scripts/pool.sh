#!/bin/bash
# 账号池统一调度入口。所有定时任务都只调这里，靠一把锁保证「任何时刻只有一个任务在跑」。
#
#   ./scripts/pool.sh claim     # 领每日签到（幂等：09:00 与 21:00 各跑一次，第二次是 already_granted）
#   ./scripts/pool.sh verify    # 只读：全池档位/额度回读，落在 data/pool_status.json
#   ./scripts/pool.sh invite    # 挑邀请人并产出「带码注册」链接，落在 data/invite_candidate.json（0600）
#   ./scripts/pool.sh onboard   # 只给未满 1500 的账号补那 5 个一次性任务
#   ./scripts/pool.sh sweep     # 扫描：谁还缺 onboarding；--apply 才补（不再尝试邀请补填）
#   ./scripts/pool.sh bind      # 【已废弃】事后补填邀请码在上游不可能，只打印说明并退出
#
# 设计约束：
#   - 互斥：data/run.lock 是目录锁（macOS 没有 flock）；拿到锁才跑，拿不到立刻退出 0，不排队、不并发。
#   - 不漏：签到跑 09:00 / 21:00 两趟，接口幂等，任意一趟成功即可；launchd 的日历任务在休眠后会补跑一次。
#   - 不重叠：所有子命令共用同一把锁，所以 21:00 的签到不可能和上一趟未结束的任务同时进行。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1
# go 只用于 claim/verify（go run 构建 cmd/activity）。别把某台开发机的绝对路径写死：
# macOS 上常是 ~/.local/go/bin，Linux 官方包是 /usr/local/go/bin，容器里多半已在 PATH。
for d in "$HOME/.local/go/bin" /usr/local/go/bin /usr/lib/go/bin; do
  [ -d "$d" ] && PATH="$d:$PATH"
done
export PATH
GO="$(command -v go 2>/dev/null || true)"

# 需要 go 的子命令在真正用到之前才检查，sweep/bind/onboard 只用 python，不该被 go 拦住。
# 协议层依赖 new-api relaykit（模块要求 go 1.25.1），tokenkit 要求 go 1.26。
need_go() {
  if [ -z "$GO" ]; then
    log "找不到 go（当前 PATH=$PATH）；claim/verify 需要 go run 构建 cmd/activity"
    log "  修复：装 Go，或把 go 所在目录加进 PATH 后重跑"
    exit 127
  fi
  local ver major minor
  ver="$("$GO" version 2>/dev/null | awk '{print $3}')"   # go1.27.3
  major="${ver#go}"; major="${major%%.*}"; minor="${ver#*.}"; minor="${minor%%.*}"
  if [ -z "$major" ] || [ "$major" -lt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -lt 26 ]; }; then
    log "go 版本太旧（$ver，需要 go1.26+：tokenkit 要求）；请升级后重跑"
    exit 1
  fi
}

LOCK="$ROOT/data/run.lock"
LOG_DIR="$ROOT/logs"
mkdir -p "$ROOT/data" "$LOG_DIR"
STAMP="$(date +%Y%m%d-%H%M%S)"
LOG="$LOG_DIR/pool-$1-$STAMP.log"

log() { printf '%s %s\n' "$(date '+%F %T')" "$*" | tee -a "$LOG"; }

acquire() {
  if mkdir "$LOCK" 2>/dev/null; then
    echo $$ > "$LOCK/pid"
    trap 'rm -rf "$LOCK"' EXIT INT TERM
    return 0
  fi
  local pid
  pid="$(cat "$LOCK/pid" 2>/dev/null || echo '')"
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    log "另一个任务正在跑（pid=${pid}），本次跳过"
    return 1
  fi
  log "回收残留锁（pid=${pid:-unknown} 已不存在）"
  rm -rf "$LOCK"
  if mkdir "$LOCK" 2>/dev/null; then
    echo $$ > "$LOCK/pid"
    trap 'rm -rf "$LOCK"' EXIT INT TERM
    return 0
  fi
  log "抢锁失败，本次跳过"
  return 1
}

prune_logs() { find "$LOG_DIR" -name 'pool-*.log' -mtime +30 -delete 2>/dev/null; }

cmd="${1:-}"
[ -n "$cmd" ] || { echo "用法: pool.sh {claim|verify|invite|onboard} [额外参数...]"; exit 2; }
shift || true
acquire || exit 0
prune_logs

case "$cmd" in
  claim)
    need_go
    log "签到开始（幂等，失败下一趟 09:00/21:00 会自动补）"
    "$GO" run ./cmd/activity -action claim -auths ./auths -delay 20s "$@" 2>&1 | tee -a "$LOG"
    rc=${PIPESTATUS[0]}
    log "签到结束 rc=$rc"
    ;;
  verify)
    need_go
    log "回读全池状态"
    "$GO" run ./cmd/activity -action status -auths ./auths -delay 5s 2>&1 | tee -a "$LOG"
    python3 scripts/invite_flow.py --verify --json 2>/dev/null \
      | tee "$ROOT/data/pool_status.json" | head -20
    chmod 600 "$ROOT/data/pool_status.json"
    log "已写入 data/pool_status.json"
    ;;
  invite)
    # 邀请关系只在注册表单提交那一刻建立，所以这里产出的是一条「带码注册」链接：
    # 新手机号必须从这条链接进注册页，事后 bind 是死路（409 existing_user_ineligible）。
    log "挑选邀请人并生成带码注册链接"
    python3 scripts/invite_link.py "$@" 2>&1 | grep -v '\[identity\]' | tee -a "$LOG"
    log "邀请链接 rc=${PIPESTATUS[0]}"
    ;;
  bind)
    # 2026-10-28 定论：POST /referrals/bind 对任何「已创建完成」的账号永久 409
    #   existing_user_ineligible（"Only a new verified phone account can bind."），
    # 判的是「这不是新号」，跟码对不对无关；注册 1 天的号也照样被拒。
    # 关系只在注册表单（/r/<code> 预填 inviteCode）提交那一刻建立 → 补填没有入口。
    log "bind 已废弃：邀请关系只能在新手机号注册时建立，已建好的账号无法补填。"
    log "  正确做法：./scripts/pool.sh invite 产出带码注册链接，新号从链接进注册页注册。"
    exit 0
    ;;
  onboard)
    # 5 个 onboarding 任务是一次性的：只给「还没满 1500」的账号补，已满的账号只做一次 summary 就跳过。
    log "onboarding 补做（仅 pending 账号；--plan 可只看不改）"
    python3 scripts/onboard_new.py "$@" 2>&1 | grep -v '\[identity\]' | tee -a "$LOG"
    log "onboarding 结束 rc=${PIPESTATUS[0]}"
    ;;
  sweep)
    # 一次性任务的总入口：扫描所有号的 onboarding 进度、邀请档位、当前带码注册链接；
    # --apply 只补 onboarding（邀请补填已证伪，不再尝试）。
    log "补做扫描（onboarding），参数: ${*:-（只扫描）}"
    python3 scripts/sweep.py "$@" 2>&1 | grep -v '\[identity\]' | tee -a "$LOG"
    log "补做扫描结束 rc=${PIPESTATUS[0]}"
    ;;
  *)
    log "未知子命令: $cmd"
    exit 2
    ;;
esac
