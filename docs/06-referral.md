# 06 · 邀请（开工搭子 / referral）

> 本篇只写实测过的内容；标注「未实测」的部分不要当结论用。

## 1. 端点

端点来自 PhanthyWork 2.43.0 macOS `claude-sidecar` 的本地活动代理路由表（`var ZK9`，
和第 03/04 篇里的 `register`/`summary`/`claim` 同一张表）：

| 本地动作键 | 方法 | 上游路径 | 幂等键 |
|---|---|---|---|
| `referral-summary` | GET | `/api/oauth/referrals/me` | 无 |
| `referral-code` | POST | `/api/oauth/referrals/code` | **必须有** |
| `referral-relations` | GET | `/api/oauth/referrals/relations?limit=N&cursor=…` | 无 |
| （客户端未暴露） | POST | `/api/oauth/referrals/bind` | 建议带 |

签名规则和每日签到完全一致：activity canonical v1、换行分隔、canonical 里的 path
含 query string、`x-desktop-*` 五个头 + `Authorization: Bearer <access_token>`。
所以 `scripts/referral.py` 直接复用 `scripts/claim_daily.py` 的 `signed_call`。

### 1.1 硬约束：`POST /referrals/code` 必须带 `idempotency-key`

实测（同一账号、同一时刻）：

```
POST /api/oauth/referrals/code  body {}  无 idempotency-key  -> 503 dependency_unavailable
POST /api/oauth/referrals/code  body {}  有 idempotency-key  -> 200 {code, invite_code, share_url, codes[5]}
```

503 的 body 是 `{"error":{"code":"dependency_unavailable","message":"Referral service is temporarily unavailable."}}`。
这不是「服务在维护」——是幂等/签名链不完整被挡。别把它当成服务端故障去重试到天亮。

## 2. 响应结构（实测）

### 2.1 `GET /referrals/me`

```json
{"schema_version":1,"referral":{
  "program_status":"active","eligible_to_invite":true,"eligibility_reason":null,
  "reward_points":5000,"reward_tiers":[5000,8000,10000],
  "tier_granted_count":0,"tier_allocated_count":0,"next_reward_points":5000,
  "daily_committed":0,"daily_granted":0,"daily_limit":50,
  "total_granted":0,"pending_count":0,"total_occupied":0,"total_limit":50,
  "remaining_slots":50,"recent_relations":[],"invited_progress":null}}
```

字段语义（结合活动页文案推断，括号里是实测值）：

- `tier_granted_count` —— **已自动到账的档位数**（0）。这是账号池「一个邀请人只做两次」的权威依据。
- `tier_allocated_count` —— 已占位（提交但未审计/未到账）的档位数。
- `next_reward_points` —— 下一档会自动到账多少（0 → 5000）。
- `reward_tiers` —— `[5000, 8000, 10000]`：前两档自动，第 3 档起人工审核。
- `daily_*` / `total_*` / `remaining_slots` —— 活动额度（单日 50、总计 50），不是个人邀请上限。
- `eligible_to_invite` / `eligibility_reason` —— 账号级资格；`false` 时不要选它当邀请人。

### 2.2 `POST /referrals/code`

```json
{"schema_version":1,
 "code":"EXAMPLE1CODE-XXXXXXXXXXXXXXXXXX",
 "invite_code":"ABCD-EFGH-JK23",
 "share_url":"https://code.phanthy.com/r/EXAMPLE1CODE-XXXXXXXXXXXXXXXXXX",
 "codes":[{"slot":1,"code":"keG…","invite_code":"ABCD-EFGH-JK23","share_url":"…/r/keG…"},
          {"slot":2,…},{"slot":3,…},{"slot":4,…},{"slot":5,…}]}
```

两层码，别混用：

- `code` —— URL 用的长 token，`/r/<code>` 走它。
- `invite_code` —— 人读码 `ABCD-EFGH-JK23`，注册表单「邀请码（选填）」填它。
- 一次返回 **5 个 slot**（`codes[1..5]`），每个 slot 一对码；本地只缓存 slot 1 当主码。

### 2.3 `GET /referrals/relations`

```json
{"schema_version":1,"relations":[],"next_cursor":null}
```

空数组 = 该账号还没有成功邀请。`next_cursor` 非空时带 `&cursor=…` 翻页。
关系项字段未实测（没有真实被邀请人），实现里按 `json.RawMessage` 透传，不做假设。

## 3. 邀请关系是怎么建立的

1. 邀请人取码：`POST /referrals/code` → `share_url = https://code.phanthy.com/r/<code>`。
2. 被邀请人打开 `/r/<code>`（实测 200，无 Set-Cookie）。HTML 里链接带
   `referral_state=rs1.<id>.<长 token>`：

   ```
   /register?next=%2Faccount%2Freferrals&referral_state=rs1.<id>.<token>…
   /login?next=%2Faccount%2Freferrals&referral_state=rs1.<id>.<token>…
   ```

   注册页把 `referralState` 放进隐藏域，和短信验证的手机号一起提交。
3. 服务端在「新手机号验证注册」那一刻绑定关系；前两档奖励自动到账，无需被邀请人先工作。

### 3.1 补填（老账号）——服务端禁止

`POST /referrals/bind`，body `{"state":"rs1.…"}`（`state` 是唯一有效字段名，实测：

| body | 结果 |
|---|---|
| `{"state":"rs1.…"}` | 409 `existing_user_ineligible` |
| `{"referral_state":"rs1.…"}` / `{"referralState":…}` / `{"code":…}` | 400 `invalid_referral_state` |

409 的原文：`Only a new verified phone account can bind.`

结论：**已经存在的账号无法事后补填邀请码**。邀请关系只能发生在注册当时，
所以「让池子里已存在的号被某个老号邀请」这条路服务端堵死了，别在脚本里反复试。
实测该账号 `tier_granted_count` 仍是 0、`relations` 仍为空——被拒的 bind 不产生任何副作用。

同一批探测里，不存在的 referral 路由（`apply`/`redeem`/`attach`/`invite`/`relation`/
`accept`/`eligibility`/`summary`）全部返回 Next.js 的 HTML 404，`/referrals/claim` 也是 404。
也就是说邀请面只有 **me / code / relations / bind** 四个端点。

## 4. 活动规则（页面原文）

- 新手机号验证注册并成功绑定邀请后，**邀请人的前两档 5,000、8,000 积分自动到账，无需首次工作**；
  第 3 档起每位 10,000 积分由人工审核发放。
- 档位按奖励承诺顺序锁定，发放受活动开关和额度限制；奖励到账后 30 天有效。
- 活动截止 2027/01/01 00:00（北京时间）。
- 被邀请人另得 2,000 新人体验积分，与邀请奖励分别记录。

## 5. 账号池调度

实现位置：

- `internal/upstream/referral.go` —— 四个端点 + `ReferralMe.AutoTiersDone()/NextAutoReward()`
- `internal/upstream/pool.go` —— `Identity.InviteBudgetLeft()/CanInvite()`、`PickInviter()`
- `internal/upstream/installation.go` —— 身份文件新增 `referralInviteCode / referralTierGranted /
  referralTotalPoints / referralNextPoints / referralEligible / referralCheckedAt`
- `cmd/activity/main.go` —— `referral` / `referral-code` / `referral-pick` / `referral-relations` / `referral-bind`

规则（按用户定的口径）：

1. 一个账号只做**前两次**自动奖励（5,000、8,000）；第 3 档起走人工审核，不进自动调度。
2. `InviteBudgetLeft() = 2 - tier_granted_count`（0/1/2，负数归零）。
3. 需要邀请人时**随机**挑一个 `CanInvite()` 为真的账号（`PickInviter`），而不是固定第一个。
4. 档位以服务端 `tier_granted_count` 为准，本地字段只是缓存；`referral-pick` 每次都先刷新 `me`。

```bash
# 巡检：每个账号的邀请档位 / 剩余次数 / 下一档奖励
go run ./cmd/activity -action referral -auths ./auths

# 取（或取回）某账号专属邀请码，结果落进身份文件
go run ./cmd/activity -action referral-code -auths ./auths

# 随机挑一个「前两档未满」的邀请人，输出可用分享链接（账号池调度入口）
go run ./cmd/activity -action referral-pick -auths ./auths

# 指定邀请人（例如固定让老号拿前两档 5,000 / 8,000）
go run ./cmd/activity -action referral-pick -auths ./auths -account <accountScope>

# 邀请关系列表（分页）
go run ./cmd/activity -action referral-relations -auths ./auths -limit 20 -cursor '<next_cursor>'

# 补填邀请码（老账号会 409，这是预期结果，不是 bug）
go run ./cmd/activity -action referral-bind -auths ./auths -state 'rs1.…'
```

Python 侧同能力（`scripts/referral.py`）：

```bash
python3 scripts/referral.py --probe            # me + relations
python3 scripts/referral.py --code             # 生成/取回邀请码
python3 scripts/referral.py --state-url https://code.phanthy.com/r/<code>   # 抓 referral_state
```

## 6. 自动化：新账号入库后自己补填邀请码

顺序不能反：

```
新账号注册 → 登录入库(auths/phanthy-*.json) → 找老号 → 拿老号的邀请码/state
          → 新账号用自己的 token + 装机签名 POST /referrals/bind → 回读老号档位 +1
```

```bash
python3 scripts/invite_flow.py --bindee <新账号scope>             # 主流程：新账号补填
python3 scripts/invite_flow.py --bindee <scope> --inviter <老号>   # 指定邀请人
python3 scripts/invite_flow.py --auto --max-age-days 7            # 近期注册且没绑过的自动补
python3 scripts/invite_flow.py --verify                           # 只读档位表
python3 scripts/invite_flow.py --pick                             # 备选：注册时带码的链接
```

- 邀请人默认挑**注册最早、前两档未满**的老号（`--random` 随机、`--inviter` 指定）；
- `referral_state` 从老号 `/r/<code>` 里取（页面多条链接，用第一个 `/register` 的那条）；
- 绑定请求由**被邀请人**签名，body 只有 `{"state":"rs1.…"}`，带 `idempotency-key`（不带会 503）；
- 成功记 `referralBoundInviter` / `referralBoundAt`；失败记 `referralBindHttp` + 服务端原文，
  `--auto` 不再自动重试被拒过的账号（`--retry` 强制）；
- 成功判据是老号 `tier_granted_count` +1 且 `relations` 里出现新账号，不是「接口 200 就算」。

### 定论（2026-10-28）：没有「补填」这条路，关系只在注册表单提交那一刻建立

| 账号 | 注册时间 | 当时年龄 | 状态 | bind 结果 |
|---|---|---|---|---|
| `acct_5f3d2c1b0a9e8d7c6b5a4f3e`（3614 分，手机号 138\*\*\*\*0000） | 2026-10-26T17:46Z | 27.9 h | onboarding 1500/1500 已做满 | **409 `existing_user_ineligible`**（"Only a new verified phone account can bind."） |

补充实测（同一次调查）：

- 字段探测：`invite_code` / `inviteCode` / `code` / `referral_code` / 空 `state` 全部
  400 `invalid_referral_state`（"Referral state is invalid or expired."），说明**只有 `state` 生效**，
  而带真实 `state` 时返回的是 409 业务拒绝 → 不是参数问题，是账号资格问题。
- `/api/daily-activity/referral-bind`、`/referrals/apply` 均 404（Next.js HTML），没有第二条补填路由。
- 前端 bundle 里搜不到「补填 / 填写邀请码 / bind」类文案或 server action：
  **Web 端没有登录后补填入口**，邀请码只在注册表单里（`<input name="inviteCode" placeholder="ABCD-EFGH-JK23">`，
  旁边写着「填写开工搭子的邀请码；通过邀请链接进入时无需填写。」）。

### 为什么它是死路

注册关系不是「账号属性上留了一个待填字段」，而是**注册事务的一部分**：

1. 注册页有一个 `<input name="inviteCode">`，从 `/r/<code>` 进入时由前端预填；
2. 服务端在「新手机号验证注册」的那一刻，用这个字段写入邀请关系，
   同时把第 1/2 档 5000 / 8000 发给邀请人；
3. 账号一旦创建完成，`POST /referrals/bind` 就再无资格 —— 409
   `existing_user_ineligible`（"Only a new verified phone account can bind."）。

所以 `existing_user_ineligible` 判的是**「这不是新号」**，不是「你填的码对不对」。
实测支持这一点：字段探测（`invite_code` / `inviteCode` / `code` / `referral_code` / 空 `state`）
全部 400 `invalid_referral_state`，带真实 `state` 时返回 409 业务拒绝 —— 参数层根本走不到；
注册才 1 天、但已经做过用量的号照样被拒。

推论：**任何「先登录入池、再回头补邀请码」的设计都是错的**，包括本仓库早期版本的
`pool.sh bind`、`bindwatch`（盯 `auths/` 目录等新号落盘就 bind）、`sweep.py --apply` 里
「先邀请后 onboarding」的顺序。这三处已在 2026-10-28 全部下线。

### 现在怎么做

```
./scripts/pool.sh invite       # 挑邀请人（前两档没用满的老号）+ 产出带码注册链接
                               # → data/invite_candidate.json（0600），看板「邀请」卡片同源
```

拿到 `https://code.phanthy.com/r/<code>` 之后，**新手机号必须从这条链接打开注册页**：
链接会把 `inviteCode` 预填进表单，注册提交时就建立关系，不需要也不能事后补。
走桌面端登录（`/ui/login.html`）建出来的账号属于"已登录"，没有注册表单这一步，
因此**永远拿不到邀请奖励**——这是产品形态决定的，不是实现缺陷。

看板上「邀请入口」卡片直接显示当前邀请人、已用档位、下一档奖励和可复制的注册链接；
定时任务里 `invite@08:50,20:50` 负责把它刷新（邀请码本身不刷新，`tier` 变了才换人）。

## 7. 未实测 / 待验证

- 真实邀请到账的时序与字段（需要一个新的手机号注册并绑定；目前 `relations` 为空）。
- `relations` 单项的字段结构（`status` / `tier` / `points` 只是从 `me.recent_relations` 的
  同名键推测，代码里没有强依赖）。
- 被邀请人 2,000 体验积分与邀请人 5,000/8,000 是否同一奖励流水。
- 第三档的「人工审核」是否有可查询的提交端点（未发现，活动页说以账户内实际状态为准）。
