# 08 选号权重：快过期积分优先被路由（三因子）

## 结论先行

网关选号从「轮询 / 字典序」改成**三因子加权抽签**：额度高的号权重高、**快过期的号权重更高**、久未使用的号有闲置补偿。
实现见 `internal/server/weight.go`，参数在 `config.json` 的 `pool` 段。

```json
"pool": {
  "idle_weight_per_hour": 0.5,
  "idle_weight_max": 5.0,
  "expiring_weight": 20.0,
  "expiry_window_days": 3.0
}
```

## 公式

```
w = 1
  + credits / maxCredits × 10                      ← 因子①额度
  + urgency × expiring_weight                      ← 因子②快过期
  + min(闲置小时 × idle_weight_per_hour, max_idle)  ← 因子③闲置
```

- `urgency = clamp(1 - 剩余天数 / expiry_window_days, 0, 1)`
  距到期 ≥ 窗口（默认 3 天）→ 0；到期当天 → 1；已过期仍按 1（不反向扣分）。
- `maxCredits` 取**全集**最大值（所有可用账号里的最高积分），不是「本次候选集」——
  workbuddy2api 的教训：按候选集算会让权重随账号被淘汰而漂移，同一账号前后权重不一致。
- 抽签用定点整数（`×1e6`，每号保底 1），避免低权重号被浮点截断成 0 概率饿死。

## 为什么"快过期"用套餐到期日

Phanthy 的积分**没有按批次过期**的字段：`activities/summary` 只给 `credits.available/reserved`，
到期时间在 `GET /api/oauth/usage/summary` 的 `plan.expires_at`（实测体验版：`2026-11-25T17:40:14Z`）。
也就是整包积分随套餐到期一起作废 → 「余量越大越怕过期」，所以用整包 `credits × urgency` 作为加成。

与 workbuddy2api 的差异（有意为之，已在代码注释里写明）：

| | workbuddy2api | 本网关 |
|---|---|---|
| 快过期口径 | `creditsExpiring / credits`（有按批次过期，只有一部分快过期） | 整包 × urgency（没有批次概念） |
| 系数 | 8.0 | **20.0**（整包过期 = 全损，风险更高） |

## 数据怎么来的

| 字段 | 来源 | 刷新时机 |
|---|---|---|
| `auth.Credits` | `activities/summary → credits.available` | 每次取 desktop token（90s 缓存）+ 每轮 claim/verify |
| `auth.PlanExpiresAt` | `usage/summary → plan.expires_at` | 每轮 claimOne（签到/巡检时） |
| `auth.LastUsed` | 上游 200 后打点 | 每次成功推理 |

三者都是**运行态**（不写进 auth 文件），进程重启后由 summary/usage 回读重建，不会因为重启把"快过期"误判成"全新"。

## 实测效果

单测（`internal/server/weight_test.go`，全绿）：

- 同样 5000 分、同样闲置：离到期 1 小时的号权重比 60 天后的号高 ≈ `expiring_weight`（20）；
- 到期日已过：仍然拿满加成（不会因为负数变成惩罚）；
- 窗口外（30 天后）：加成 0，与"没有到期信息"的号权重相同；
- 从未使用的号：拿满 `idle_weight_max`；闲置 1000 小时也不超过封顶；
- 分母（maxCredits）变小 → 低额度号权重上升；
- 抽签 2000 次：2 小时后到期 vs 90 天后到期，前者被选中 ≈ 74%（>70% 断言通过）。

## 怎么调

- 想让"快过期"更强势：调大 `expiring_weight`（20 → 40 意味着紧迫号几乎必选）；
- 想提前更多天开始倾斜：调大 `expiry_window_days`（3 → 7）；
- 想回到"额度优先、过期不管"：`expiring_weight = 0`；
- 冷却/失败账号仍然先被排除（`usableAccounts` + `cooling`），权重只在可用集合内比较。

## 未做

- 没有把权重暴露到 `/status`（只有 `tally` 的命中统计）；要看当前权重值需要临时加日志。
- Python 侧脚本（`cmd/activity`）仍然是"随机挑邀请人"，权重只作用于**推理选号**，不影响签到/邀请调度。
