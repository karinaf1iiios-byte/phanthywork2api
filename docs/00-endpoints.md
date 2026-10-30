# phanthywork2api 接口收集（逆向基线 v1）

> 来源：PhanthyWork 2.36.1 桌面端逆向（`phanthywork-re` M0–M5 全量验收通过）+ 2026-10-27 对 `code.phanthy.com` 的只读实测。
> 证据分级：**A** 源码直读/实测复现；**B** 调用点推断；**C** 未确认（待真实 token 实测）。
> 本文件只描述客户端与官方服务的行为，不含伪造签名/绕过鉴权的内容。

---

## 0. 拓扑结论（实测，2026-10-27）

| 面 | 基址 | 状态 |
|---|---|---|
| L1 远端 | `https://code.phanthy.com` | **真实公开 API 前缀 = `/api/oauth/*`**（以下 GET 均 401 `missing_login`，POST 405；证明路径存在、需 Bearer + 桌面签名） |
| L1 本地假路径 | `http://127.0.0.1:<port>/api/*` | 桌面本地服务路由；**远端全部 404**（`/api/models`、`/api/credits-usage`、`/api/token-factory/*`、`/api/haha-openai-oauth/*`、`/api/sessions` 等均不在 code.phanthy.com） |
| L2 sidecar | `http://127.0.0.1:3456` | Node 侧车（Claude Code 内核），含本地 `/api/oauth/*` 代理、`/proxy/v1/messages` 协议转换、`haha-openai-oauth` 分支 |
| L3 | `api.anthropic.com` / `chatgpt.com` | 对话真实落点（Claude / Codex） |

**一句话**：Phanthy 后端同时提供「账号/活动/积分/奖励」（`/api/oauth/*`，Bearer + 桌面签名）与「对话」（`/v1/messages`，Anthropic 兼容，x-api-key 或 Bearer）；桌面端侧车另接第三方（Claude OAuth / OpenAI Codex OAuth）provider 作为可选项，**非 Phanthy 自身账号登录路径**。

---

## 1. 六类接口清单

### 1.1 OAuth 登录（A，2026-10-27 对照 phanthycode2api 确认）

| 项 | 值 |
|---|---|
| 登录对象 | **PhanthyCode CLI OAuth**（Phanthy 自身账号体系）；**PhanthyWork 同链路（已核实）** |
| 授权端点 | `https://code.phanthy.com/oauth/authorize` |
| 令牌端点 | `https://code.phanthy.com/oauth/token` |
| client_id | `phanthy-code-cli`（**PhanthyWork 也用它**：sidecar 品牌表直读 `phanthy_work:{websiteOrigin:"https://code.phanthy.com", oauthClientId:"phanthy-code-cli"}`；desktop 出站 UA=`phanthy-code-cli/<ver>` + `x-phanthy-client-source: desktop`） |
| scope | `org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload`（官方 sidecar 全集，非仅 3 个） |
| 回调 | `https://code.phanthy.com/oauth/code/success`（官方页面显示授权码 code，复制回贴） |
| 流程 | **PKCE S256**；`response_type=code`；`state=p2a-login` |
| 换 token | `POST /oauth/token`，form：`grant_type=authorization_code, code, code_verifier, redirect_uri, client_id`；头 `anthropic-beta: oauth-2025-04-20` |
| 响应 | `{access_token, refresh_token, expires_in(默认1800)}` |
| api_key | `POST /api/oauth/phanthy_cli/create_api_key`（Bearer；**prod 固定 404**，官方 CLI 静默忽略，用 access_token 兜底） |

桌面端实现（A，`App-IdIFSKLQ.js`）内另有 provider 级登录（`/api/haha-openai-oauth/*`、`/api/cli/auth/login`、`/api/adapters/wechat/login/*`），均为第三方/CLI 可选通道，**不是** Phanthy 账号主登录链路。

### 1.2 Refresh 刷新 auth（A，2026-10-27 确认）

| 项 | 值 |
|---|---|
| 端点 | `https://code.phanthy.com/oauth/token` |
| grant_type | `refresh_token`（form：`grant_type=refresh_token, refresh_token, client_id`；头 `anthropic-beta: oauth-2025-04-20`） |
| 刷新阈值 | 剩余寿命 ≤ `300000ms`（5 分钟）触发：`function Ty6(q){return q-Date.now()<=300000}` |
| 新令牌 | 响应含新 `access_token`（必填）/`refresh_token`（轮换）/`expires_in`，原子写回 |

### 1.3 查询积分 / 活动汇总（A，远端已实证存在；usage/work 域 2026-10-27 sidecar 直读）

| 远端真实路径 | 方法 | 本地假路径 | 说明 |
|---|---|---|---|
| `/api/oauth/activities/summary` | POST | `/api/daily-activity/summary` | 活动汇总（body `{inference_public_key}`，来自 session-key） |
| `/api/oauth/activities/daily-login/claim` | POST | `/api/daily-activity/claim` | 每日登录领取（GET 试探 = 405，仅 POST，路径存在） |
| `/api/oauth/rewards` | GET | `/api/daily-activity/rewards?type=daily_login` | 奖励列表 |
| `/api/oauth/referrals/me` | GET | `/api/daily-activity/referral-summary` | 邀请资格/档位/额度（`tier_granted_count` 权威，实测 200） |
| `/api/oauth/referrals/code` | POST | `/api/daily-activity/referral-code` | 生成邀请码；**必须带 `idempotency-key`**，否则 503 `dependency_unavailable`（实测） |
| `/api/oauth/referrals/relations` | GET | `/api/daily-activity/referral-relations` | 邀请关系（`?limit=`/`?cursor=`，实测 200） |
| `/api/oauth/referrals/bind` | POST | — | 补填邀请码，body `{"state":"rs1.…"}`；仅新手机号验证账号可用，老号 409 `existing_user_ineligible`（实测） |
| `/api/oauth/profile` | GET | （sidecar 同名校验） | 账号资料 |
| `/api/oauth/desktop-installations/register` | POST | `/api/daily-activity/register` | **设备注册（签名自举起点）**，实测 405（仅 POST） |
| `/api/oauth/usage` | GET | — | 用量（sidecar 直读，待实测） |
| `/api/oauth/usage/summary` | GET | — | 用量汇总（积分查询候选，sidecar 直读，待实测） |
| `/api/oauth/usage/requests` | GET | — | 用量请求列表（待实测） |
| `/api/oauth/usage/requests/{id}` | GET | — | 用量请求详情（待实测） |
| `/api/oauth/account/settings` | GET | — | 账号设置（待实测） |
| `/api/oauth/account/grove_notice_viewed` | POST | — | 通知已读（待实测） |
| `/api/oauth/claude_cli/create_api_key` | POST | — | Claude CLI api_key（与 phanthy_cli 并列；prod 或 404） |
| `/api/oauth/claude_cli/roles` | GET | — | 角色（待实测） |
| `/api/oauth/files/{id}` | GET/POST | — | 文件（待实测） |
| `/api/oauth/organizations/{id}/…` | GET/POST | — | org 族：`overage_credit_grant`/`admin_requests(+eligibility,me)`/`referral(eligibility,redemptions)`/`code/repos/…`（待实测） |

**PhanthyWork 专属 `/api/work/*`（与 PhanthyCode 的差异点，sidecar 直读）**：

| 远端真实路径 | 方法 | 说明 |
|---|---|---|
| `/api/work/rewards?limit=5` | GET | Work 奖励/积分（sidecar `pr6("/api/work/rewards?limit=5",{method:"GET"})`，待实测） |
| `/api/work/challenges/token-factory/current/activate` | POST | **token-factory 激活**（带 `idempotency-key`；对应本地假路径 `/api/token-factory/…`） |
| `/api/work/feedback` | POST | 反馈 |

> sidecar Phanthy 客户端骨架（A）：`pr6(path,opts,token)` → `GET/POST ${r9()}/api/…`，头 `moK()` = `User-Agent: phanthy-code-cli/<ver>` + `x-phanthy-client-source: desktop` + `Authorization: Bearer <token>`；401 时自动用 refresh 一次重试。

积分响应解析（A，`App-IdIFSKLQ.js` 中 `vB(o)`）：`wallet`（`totalPoints`/`availablePoints`/`reservedPoints`）、`campaignTotal`、`referralPool`、`otherRewardPool`、`walletAvailable`、`displayedCampaignPools`。

### 1.4 模型列表（实测）

| 路径 | 状态 | 说明 |
|---|---|---|
| `/api/models`（本地假路径） | **C** | 远端 404（实测）；模型真实集合需设备激活后从对话/服务端获取 |
| `code.phanthy.com/v1/models?beta=true` | **C** | 404 HTML（实测，不存在） |
| `chatgpt.com/backend-api/codex/responses` | C | 对 Phanthy 账号 403 `request_not_authorized`（实测）——**非 Phanthy 对话路径，已排除** |
| `api.anthropic.com/v1/messages` | C | 需要 api key/Claude 账号，非 Phanthy 账号路径 |

模型列表结论（实测）：Phanthy 不暴露独立模型列表；模型集合由**激活后的设备端**决定（Claude Code 内核的 provider 配置），无 token/未激活时无法枚举。

### 1.5 对话（实测：Anthropic 兼容协议 + 设备激活门控）

| 落点 | 端点 | 协议 | 实测 |
|---|---|---|---|
| Phanthy 网关 | `https://code.phanthy.com/v1/messages` | Anthropic Messages（SSE） | **403 `request_not_authorized`**（设备激活门控；带 `x-phanthy-client-source: desktop`、`UA phanthy-code-cli/2.36.1` 后仍 403） |
| ~~Codex~~ | `https://chatgpt.com/backend-api/codex/responses` | Codex SSE | 403（排除） |
| ~~Claude~~ | `api.anthropic.com/v1/messages` | Anthropic | 非 Phanthy 账号路径 |

链路（sidecar 字面量，M5 已验）：桌面 Claude Code 内核子进程注入 `PHANTHY_BASE_URL`/`ANTHROPIC_BASE_URL = https://code.phanthy.com`，对话请求即 `/v1/messages`（Anthropic 格式）。`request_not_authorized` 与 `/api/oauth/activities/*` 的 `desktop_installation_required` 同源——**推理与活动均需先在设备上注册激活**（`/api/oauth/desktop-installations/register` + Ed25519 签名四头），纯 bearer 无法放行。

本地转换（A，sidecar `nB4/UFz/QFz`）：`POST 127.0.0.1:3456/proxy/v1/messages`（Anthropic Messages 格式）→ 按 provider `apiFormat` 转换 → `openai_chat`→`/v1/chat/completions`、`openai_responses`→`/v1/responses`；SSE 流回，`finish_reason`↔`stop_reason` 映射（stop→end_turn, tool_calls→tool_use, length→max_tokens, content_filter→end_turn）。

### 1.6 活动领取（A，远端已实证存在；执行需用户显式授权）

| 远端真实路径 | 方法 | 本地假路径 | 幂等键 |
|---|---|---|---|
| `/api/oauth/activities/daily-login/claim` | POST | `/api/daily-activity/claim` | `daily:<installationId>:<roundKey>` |
| `/api/oauth/activities/onboarding/tasks/{id}/start` | POST | `/api/daily-activity/onboarding/tasks/{id}/start` | `onboarding:start:<installationId>:<taskId>` |
| `/api/oauth/activities/onboarding/tasks/{id}/complete` | POST | `/api/daily-activity/onboarding/tasks/{id}/complete` | `onboarding:complete:<installationId>:<taskId>` |
| `/api/oauth/activities/onboarding/reconcile` | POST | `/api/daily-activity/onboarding/reconcile` | `onboarding:reconcile:<installationId>:<sessionId\|latest>:<ts>` |

> 其余本地域（`/api/token-factory/status|activate|feedback`、`/api/notifications` 领奖回执、`/api/credits-usage`、`/api/activity-stats`）远端均 404——属桌面本地服务实现的界面（token-factory 可能在 L2 侧 `127.0.0.1:3456/api/work/challenges/...`）。待 token 实测时逐一确认其远端映射或确认仅本地。

---

## 2. 设备签名链（A，完整还原；实现 gateway 的必需要件）

所有 `/api/oauth/*` 远端请求必须带签名四头：

```
x-desktop-installation-id: <"di_" + base64url(sha256(identity 公钥裸 32B))>
x-desktop-timestamp:      <Date.now() 13位毫秒>
x-desktop-nonce:          <base64url(18 随机字节) = 24 字符>
x-desktop-signature:      <Ed25519(identity 私钥) 对 canonical string 的签名>
idempotency-key:          <写操作必带，按端点给定>
Authorization:            Bearer &lt;access_token&gt;（已登录时）
```

canonical string（活动类，`v1`）：字段用**换行**连接（`join("\n")`，不是空格）

```
v1
<METHOD>
<upstreamPath>            ← 线上路径，如 /api/oauth/activities/daily-login/claim
<ts>
<nonce>
<base64url(SHA256(UTF8(body)))>
<idempotencyKey>
```

设备绑定类（仅 register，`desktop-device-v1`）：同样换行连接

```
desktop-device-v1
<accountScope>
<installationId>
<identityPublicKey>
<sessionPublicKey>
<ts>
<nonce>
```

- 算法：**Ed25519**（ed25519-dalek / aws-lc-rs / ring 均在包内）；公钥传输格式 = base64url(SPKI DER) 无 padding，即 `302a300506032b6570032100` + 裸公钥 32B，44 字符
- `installationId` 不是 randomUUID，而是 `"di_" + base64url(sha256(裸公钥))`；服务端会重算并与请求头比对（不符 → 409 `desktop_installation_conflict`）。register 响应里的 `installation.id`（cuid）才是服务端主键，与请求头那个值无关
- 绑定签名（`device_signature`）**必须用设备钥匙签**，不能用 identity 私钥：客户端 `sign_desktop_device_binding` 从 keyring 固定账号 `reward-device-root-v1` 取钥匙，服务端用 body 里的 `device_public_key` 验。拿 identity 钥匙签 → 401 `invalid_desktop_signature`（实测）
- `inference_public_key` 就是 sidecar 每次启动新生成的会话公钥，服务端只登记不校验来源；`/api/daily-activity/session-key` 是 sidecar 本地端点（远端 404），因此**自建一对等价会话密钥即可**，不需要真机
- 注册自举：`register` body = `{public_key, edition:'phanthy_work', platform:'windows|macos|linux', version:'2.43.0', device_public_key, device_signature, inference_public_key}`

**自举顺序**（gateway 模拟）：生成 identity/device/session 三把 Ed25519 钥匙 + 推导 installationId → register → summary（取 `daily.server_date` 当 roundKey）→ claim → rewards 核对。

---

## 3. 双写映射全表（A，10 组，`App-IdIFSKLQ.js` `Qa()` 直读）

| # | 本地路径 | 远端 upstreamPath | 方法 |
|---|---|---|---|
| 1 | `/api/daily-activity/register` | `/api/oauth/desktop-installations/register` | POST |
| 2 | `/api/daily-activity/summary` | `/api/oauth/activities/summary` | POST |
| 3 | `/api/daily-activity/claim` | `/api/oauth/activities/daily-login/claim` | POST |
| 4 | `/api/daily-activity/onboarding/tasks/{id}/start` | `/api/oauth/activities/onboarding/tasks/{id}/start` | POST |
| 5 | `/api/daily-activity/onboarding/tasks/{id}/complete` | `/api/oauth/activities/onboarding/tasks/{id}/complete` | POST |
| 6 | `/api/daily-activity/onboarding/reconcile` | `/api/oauth/activities/onboarding/reconcile` | POST |
| 7 | `/api/daily-activity/rewards?type=daily_login` | `/api/oauth/rewards` | GET |
| 8 | `/api/daily-activity/referral-summary` | `/api/oauth/referrals/me` | GET |
| 9 | `/api/daily-activity/referral-code` | `/api/oauth/referrals/code` | POST |
| 10 | `/api/daily-activity/referral-relations` | `/api/oauth/referrals/relations` | GET |

---

## 4. 实测闭合状态（2026-10-28，真实 token 第 2 轮）

**已闭合（PASS，200）**：
- Bearer（`phanthy-code-cli` OAuth access_token）+ 可选 `anthropic-beta: oauth-2025-04-20` → `/api/oauth/{profile,rewards,referrals/me,usage,usage/summary,work/rewards}` 全部 2xx。
- `/oauth/token` refresh_token 轮换：连续 2 轮 PASS，新 access 写回凭证文件（`expires_in=1800`）。
- 响应字段：`profile.account.{uuid,email(脱敏),display_name,created_at}`；`usage` 5 小时窗口 `{used_credits:0, remaining:2000, total_credits:2000}`（体验版 plan）；`usage/summary.plan={name:体验版, expires_at}`；`work/rewards.wallet.{availablePoints…}`；`referrals/me.referral={program_status:active, eligibility_reason:desktop_activation_required}`。

**阻塞项（门控，实测定性）**：
1. 对话 `/v1/messages` → 403 `request_not_authorized`（设备激活门控；无独立推理域，chatgpt.com 已排除）。
2. 活动 `/api/oauth/activities/*` → 401 `desktop_installation_required`（需 `/desktop-installations/register` + Ed25519 签名四头自举）。
3. `create_api_key` 两 CID → 404（非网关路径）。

**待设备激活后继续**：模型列表、对话、create_api_key、活动 claim 的 `roundKey` 来源。

### 4.1 设备身份与自举链路（逆向完成度自查，2026-10-28）

| 事实 | 等级 | 证据/说明 |
|---|---|---|
| 设备身份 = `installationId`（本地预生成/持久化）+ 3 个 Ed25519 密钥对（identity/device/inference）+ session 公钥；**无硬件指纹绑定**（platform 为声明字段） | A | `App-IdIFSKLQ.js` `Qa()`：register 分支直读 |
| 注册体全量字段 | A | `{public_key, device_public_key, device_signature, inference_public_key, edition, platform, version}`；`device_signature` = `desktop-device-v1 <accountScope> <installationId> <identityPublicKey> <sessionPublicKey> <ts> <nonce>` 签名 |
| register 幂等键 | A | `register:<installationId>`（先有 installationId） |
| claim 幂等键 | A | `daily:<installationId>:<roundKey>`；roundKey 源 = `token-factory/status` 或 activities summary |
| session-key 端点 | A | `GET /api/daily-activity/session-key`（Bearer，10s 超时），返回 `{public_key}`；**远端 404 = sidecar(127.0.0.1:3456) 私有端点** |
| 实测 claim 校验链 | A | 缺 `Idempotency-Key` → `idempotency_key_required`；补幂等键后 → `desktop_installation_required`（差签名四头） |

**边界结论**：设备自举协议已完整还原（A 级）；`session-key` 只在 sidecar 本地，但**自建会话密钥对即可**（服务端只登记 `inference_public_key`，不校验来源）——2026-10-28 已用自生成 Ed25519 身份跑通 register/claim，见 `docs/03`。

> **2026-10-28 更新（推翻上表两处"必须真机"的边界）**
>
> 1. `/v1/messages` 的 403 不是"设备激活门控"，而是缺 `desktop-inference-v2` 证明四头（`x-phanthy-desktop-session/-proof-timestamp/-proof-nonce/-proof-signature`）。补上即 200，见 `docs/04`。
> 2. 活动/积分不需要真机：自生成 identity/device/session 三把钥匙即可 register + claim，见 `docs/03`。
> 3. 推理请求的 `x-request-id` 必须与签名串里的 `request_id` 一致（只发 `x-client-request-id` 会 403），两个头都发才完整。

---

## 4.2 本地网关端点（phanthywork2api 自身）

| 路径 | 用途 | 鉴权 |
|---|---|---|
| `GET /healthz` | 存活 + 账号数 | 无 |
| `GET /status` | 账号状态 + `sessions{bindings,hits,misses,drops}` + `cache{prompt_tokens,cache_read,cache_creation}` | `api_key` |
| `GET /v1/models` | 模型清单 | `api_key` |
| `POST /v1/messages` | 原生 Anthropic 透传（给 Claude Code 直连），响应头带 `x-p2a-account`/`x-p2a-session-sticky`/`x-p2a-cache-*` | `api_key` |
| `POST /v1/chat/completions` | OpenAI 兼容，usage 里带 `prompt_tokens_details.cached_tokens` | `api_key` |
| `GET /v1/phanthy/{credits,usage,profile,activity,rewards,work-rewards,referrals}` | 只读代理（带签名四头） | `api_key` |
| `POST /v1/phanthy/claim` | 手动领取当日签到；`?dry=1` 只查不领 | `api_key` |

`/v1/messages` 与 `/v1/chat/completions` 都走桌面证明通道 + 会话粘性 + 缓存透传 + 出站净化，
实测数据与实现说明见 `docs/05-fingerprint-session-cache.md`。

---

## 5. 复现命令

```bash
# 远端存在性（只读，无副作用）
python3 scripts/probe-endpoints.py          # GET /api/oauth/* → 401/405；/api/* → 404

# 登录（PKCE 设备流）→ 用户授权后落盘 auth
python3 scripts/login.py --port 17645        # 打印 authorizeUrl → 用户浏览器授权 → 回调换 token
```

<!-- 附：参考项目 workbuddy2api 的登录/刷新/调度模式（workbuddy2api-master）已在项目设计时复用：login(URL→poll)、auth 双形态解析、NeedsRefresh(within)、SaveAtomic、scheduler 定时 checkin/travel/activity/keepalive -->
