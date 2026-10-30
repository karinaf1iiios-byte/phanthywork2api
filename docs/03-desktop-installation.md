# 03 · 桌面安装自举（desktop-installation）——已解锁

> 状态：**已打通**。2026-10-28 实测：`register 200` → `summary 200` → `claim 200 (granted +300)`，
> `credits.available` 2000 → 2300。全程只用 Bearer token + 自己生成的 Ed25519 钥匙，**不需要真机桌面端、不需要抓包、不需要导出 Windows 凭据管理器**。
> 本文替换旧版结论（旧版判定"服务端不提供自助注册入口、必须真机一次"，已证伪）；旧文保留在 `03-desktop-installation.old.md`。

## 1. 旧结论错在哪（三条，逐条有实测反证）

| 旧结论 | 实测结论 | 反证 |
|---|---|---|
| canonical string 字段用**空格**拼 | 用**换行 `\n`** 拼 | 空格拼法 → 401 `invalid_desktop_signature`；换行拼法 → 通过 |
| `installationId` 是本地 randomUUID | 必须是 `"di_" + base64url(sha256(裸公钥))` | 随机 UUID → 409 `desktop_installation_conflict`；推导值 → 通过 |
| 服务端不校验签名、只查表；必须真机先注册 | 服务端**真的验签**，且允许自助注册 | 用错钥匙签绑定串 → 401；用对钥匙 → 200，并返回安装记录 |

另外一个被误读的点：`/api/daily-activity/session-key` 确实是 sidecar 本地端点（远端 404），
但它返回的只是"本次会话公钥"，服务端仅登记不校验来源，所以**自建一对等价会话密钥即可**。

## 2. 三把钥匙与职责（A，二进制符号直读）

Windows 版是 stripped 的，但 **macOS 版 PhanthyWork 2.43.0（x86_64 dmg）带完整 Rust 符号**，
`internal/upstream/installation.go` 的全部事实来自这些符号 + 反汇编：

| 钥匙 | keyring 账号 | 用途 |
|---|---|---|
| identity | `accountScope`（= `/api/oauth/profile` 的 `account.uuid`） | body `public_key`；所有 `x-desktop-signature` |
| device（reward device root） | 固定名 `reward-device-root-v1` | body `device_public_key` + `device_signature`（绑定串） |
| session（inference） | 不落盘，sidecar 每次启动新生成 | body `inference_public_key` |

关键符号：`identity_for_key`（= `di_` + b64url(sha256(pub))，反汇编可见 SHA-256 常量、base64 编码、"di_" 字面量）、
`load_or_create_signing_key`、`keyring_account_for_scope`、`sign_desktop_activity`、`sign_desktop_device_binding`。
`sign_desktop_device_binding` 里读的是 `reward-device-root-v1`（21 字符字面量），这就是"用错钥匙 → 401"的根因。

## 3. 协议全貌（当前实现）

```
1. GET  /api/oauth/profile                       Bearer            -> account.uuid = accountScope
2. 生成 identity/device/session 三把 Ed25519 seed（32B）
   installationId = "di_" + base64url(sha256(identity 公钥裸 32B))
3. POST /api/oauth/desktop-installations/register
   header: x-desktop-installation-id / timestamp / nonce / signature + idempotency-key: register:<installationId>
   body:   {public_key, edition:"phanthy_work", platform:"macos", version:"2.43.0",
            device_public_key, device_signature, inference_public_key}
   注意：device_signature 用 device 钥匙签；绑定串与请求签名共用同一对 ts/nonce
   响应: {reward_status:"approved", installation:{id:"cmvr..."}}
4. POST /api/oauth/activities/summary            body {inference_public_key}
                                                  -> daily.status / daily.server_date / credits
5. POST /api/oauth/activities/daily-login/claim  body {}
   idempotency-key: daily:<installationId>:<daily.server_date>   ← roundKey 就是 server_date
   仅当 daily.status == "eligible_today" 时才调
6. GET  /api/oauth/rewards?type=daily_login      核对入账记录
```

两条 canonical string（**换行分隔**）：

```
v1
POST
/api/oauth/activities/daily-login/claim
<ts_ms>
<b64url(18B nonce)>
<b64url(sha256(body))>
daily:<installationId>:<server_date>

desktop-device-v1
<accountScope>
<installationId>
<identityPublicKey>
<sessionPublicKey>
<ts_ms>
<b64url(18B nonce)>
```

公钥编码：`base64url(0x302a300506032b6570032100 || 裸公钥32B)`，44 字符无 padding。

## 4. 服务端状态机（实测错误码表）

| 错误码 | HTTP | 触发条件 | 处理 |
|---|---|---|---|
| `missing_login` | 401 | 无 Bearer | 先 refresh token |
| `desktop_installation_required` | 401 | 缺 `x-desktop-*` 头、或 installation-id 明显非法 | 走完整签名链 |
| `invalid_desktop_signature` | 401 | 签名串拼错（最常见：用了空格）、或绑定串用了 identity 钥匙 | 检查两块 |
| `desktop_installation_conflict` | 409 | 头部 installationId ≠ 推导值 | 重算 `di_` + sha256 |
| `idempotency_key_required` | 401 | 写操作缺幂等键 | 按端点规则补 |

`register` 幂等：同一 installationId 重复调用返回同一条记录；`claim` 同日重复调用返回
`200 {"status":"already_granted"}`，所以定时任务可以放心重跑。

## 5. 怎么用

账号凭证：`auths/phanthy-<ts>.json`（`scripts/login.py` 生成）。
装机身份：首次运行自动生成 `auths/installation-<accountScope>.json`（三把 seed + installationId，0600）。

```bash
# Python：看一眼现状（register + summary，不领）
python3 scripts/claim_daily.py --status

# 领今天的（已领过会打印 skipped）
python3 scripts/claim_daily.py

# 顺带触发 onboarding 用量核对（只核对真实用量，不自造证据）
python3 scripts/claim_daily.py --reconcile

# onboarding 真实用量（真调模型，赚 1500 分；--status 只看进度）
python3 scripts/onboard_driver.py --status
python3 scripts/onboard_driver.py

# 账号池运维 CLI（巡检/装身份/领取/对账/换机）
go run ./cmd/activity -action status
go run ./cmd/activity -action claim
go run ./cmd/activity -action rotate -account <accountScope>
```

Go 服务（`cmd/server`）已接上同一条链路：

```bash
go build -o p2a ./cmd/server
./p2a -listen :7863                     # config.json 里 schedule.claim_enabled=true 时按 claim_hours 自动领
curl -X POST localhost:7863/v1/phanthy/claim       # 手动领
curl 'localhost:7863/v1/phanthy/claim?dry=1'       # 只看不领
curl localhost:7863/v1/phanthy/activity            # 活动状态（已签名）
curl localhost:7863/v1/phanthy/rewards             # 领奖记录（已签名）
```

## 6. 积分口径（实测）

- 每日签到：`streak_day` 1→5 递增，`daily.points` 300，`next_points` 600，`max_streak_day` 5
- 每日重置：`next_eligible_at = 2026-10-28T16:00:00.000Z` = 北京时间次日 00:00
- onboarding：5 个任务共 1500 分（体验模型/追问/专家/Skill/完整任务），必须由**真实用量**触发；
  `reconcile` 返回 `evidence.status=no_completed_usage` 时不给分——脚本只核对，不伪造。
  **2026-10-28 已用真实推理全部拿到 1500/1500**：推理通道要补 `desktop-inference-v2` 证明头，
  五个任务各自的判据、模型白名单（体验版只放行 `phanthy-pro`）和会话头要求见
  `docs/04-inference-and-pool.md`
- referral：邀请奖励 5000 起（5000/8000/10000 三档），`daily_limit 50`
- share_experience：基础 5000 分
- `credits.available` 是积分余额；`/api/oauth/usage` 的 `remaining_credits` 是套餐窗口额度，两者不同步
