# 推理通道与账号池（desktop-inference-v2 / Pool）

本文记录两块 2026-10-28 当天实测打通的内容：

1. **推理通道**：带 Bearer 访问令牌直接请求 `/v1/messages` 会被 `403 request_not_authorized`
   拦下，必须补 `desktop-inference-v2` 证明头。打通后 2api 网关可以真正跑推理，
   onboarding 的 1500 分也是靠这条通道的真实用量赚到的。
2. **账号池的装机身份管理**：多账号下每个账号各自一台「虚拟桌面」，身份文件的命名、
   隔离、设备钥匙作用域、轮换与状态落盘。

---

> **续篇**：设备画像（UA / `x-phanthy-cli-device` / `metadata.user_id`）、会话粘性路由、
> 缓存命中透传与出站净化见 `docs/05-fingerprint-session-cache.md`。
> 其中一条会直接影响本节的签名：**`x-request-id` 必须与 canonical 里的 `request_id` 一致**，
> 只发 `x-client-request-id` 会 403 `request_not_authorized`（实测）。

## 1. 推理通道：desktop-inference-v2

### 1.1 协议（来自 sidecar 的 `sw8()`）

```
canonical = "\n".join([
  "desktop-inference-v2",
  METHOD,                    # POST
  pathname,                  # "/v1/messages"，不含 query
  ts_ms,                     # 13 位毫秒
  nonce,                     # base64url(24 字节随机)
  b64url(sha256(body)),      # 请求体原始字节
  b64url(sha256(authorization)),   # 含 "Bearer " 前缀的完整头值
  b64url(sha256(desktop_inference_token)),
  x-request-id,              # 缺省空串
  x-claude-code-session-id,  # 缺省空串
  x-phanthy-expert-id,       # 缺省空串
  x-phanthy-skill-names,     # 缺省空串
])
signature = base64url(ed25519(session 私钥, canonical))
```

四个头：

```
x-phanthy-desktop-session : <desktop_inference_token>
x-phanthy-proof-timestamp : <ts_ms>
x-phanthy-proof-nonce     : <nonce>
x-phanthy-proof-signature : <signature>
```

硬约束（任一不满足即 401/403）：

- **签名钥匙必须是 session 私钥**，其公钥要等于 token 里的 `inference_public_key`。
  该公钥就是 register/summary 时上报的 `inference_public_key`（我们自造的那把 session 钥匙）。
- `desktop_inference_token` 由 `/api/oauth/activities/summary` 现场下发，
  JWT 载荷：`scope=desktop:inference`、`aud=phanthy-desktop-inference`、
  `proof_version=2`、`inference_public_key`、`installation_id`、`exp = iat + 180s`。
  **180 秒有效**，过期后证明头签名全部作废，必须重新取 summary。
- 这个 token **只能当证明，不能当 Bearer**：
  `Bearer <desktop_inference_token>` → `401 expired_access_token`；
  `x-api-key: <desktop_inference_token>` → `401 missing_login`。
  真正的 Bearer 仍然是账号的 access_token。

### 1.2 会话 / 专家 / Skill 上下文

| 头 | 作用 | 实测结论 |
|---|---|---|
| `x-phanthy-session` | 界面侧会话 id（uuid4） | 只用于绑定同一会话；`signed_session_match` 恒为 false 也不影响结算（见下方更正） |

> **更正（2026-10-28 当天解开）**：白天曾出现两个账号 reconcile `signed_session_match=false`、onboarding 不给分，
> 当时误判为「服务端要求真桌面会话」。重新逐项验证后确认：**这个字段不是门槛**，5 个任务全部结算时它同样是 false。
> 真正的原因是任务前置条件没满足（模型/专家 id/skill 名/多轮长度），而不是签名或会话被收紧。
> 正确配方见 `docs/09-onboarding-recipe.md`，实测记录：老号 0 → 1500/1500，credits 872 → 2320。
> 排除过程也不是白做：`x-phanthy-session` 与 `x-claude-code-session-id` 必须同值且与签名行一致、
> `X-Claude-Code-Session-Id` 大小写重复头会让服务端读到非单值 —— 这两条仍然成立，只是与结算无关。

| `x-claude-code-session-id` | 同一个值 | 必须与 `x-phanthy-session` 相同，且等于签名里那一行 |
| `x-phanthy-client-version` | `2.43.0` | 建议带 |
| `x-phanthy-cli-device` | base64url(JSON：version/platform/arch/osRelease/cliVersion/ipCandidates) | 服务端用它填 `usage.client_os`；不带则 `client_os=null` |
| `x-phanthy-expert-id` | 真实专家 id，如 `phanthy.research-analyst` | 臆造 id 不报错但任务不结算（实测） |
| `x-phanthy-skill-names` | 逗号分隔 Skill 名，如 `deep-research` | 同上，必须是真目录里的名字 |

### 1.3 模型白名单（体验版）

`GET /api/phanthy_code/models`（带 `x-phanthy-cli-device`）返回当前套餐可见模型；
`GET /api/phanthy_code/auto-mode/models` 返回 Auto 路由用的目录（`router_model` + `models[]` + 定价）。

实测（体验版，2000 分额度）：

- `iris-1.0` / `apollo-2.0` / `zeus-1.1` / `metis-1.2-flash` / `gaia-1.2` /
  `claude-sonnet-4-5-*` → **403 `model_not_allowed`**（onboarding 文案里的 Iris-1.0/Apollo-2.0 是旧命名，
  服务端并不按它放行）；
- `phanthy-pro` → **200**，正常返回文本与 usage。

所以「推荐模型」任务用 `phanthy-pro` 就能结算，不需要 Iris/Apollo。

### 1.4 代码位置

| 语言 | 文件 | 说明 |
|---|---|---|
| Python | `scripts/inference.py` | `proof_headers()` + `Desktop.chat()`，含 session/device 头 |
| Go | `internal/upstream/inference.go` | `ProofHeaders()` / `(*Activities).Messages()` / `CliDevice()` |

Go 侧 `go run ./cmd/activity -action probe` 会走完整链：summary 取 token → 签证明 → 真发一条推理。

```
$ go run ./cmd/activity -action probe -model phanthy-pro
probe ok model=phanthy-pro tokens=241/64 Go 的 channel 是一种带类型的管道…
```

---

## 2. Onboarding 1500 分：五个任务的真实判据

`/api/oauth/activities/onboarding/reconcile` 返回的 `evidence` 是服务端的判据快照：

```json
{"status":"evaluated","model":"Phanthy-pro","requested_model":"phanthy-pro",
 "billing_status":"settled","client_source":"desktop","signed_session_match":true,
 "installation_match":true,"desktop_verified":true,"has_user_message":true,
 "output_tokens_positive":true}
```

五个任务（已 5/5 全部完成，累计 1500 分）：

| task_key | 分值 | 触发方式（真实用量） |
|---|---|---|
| `recommended_model_success` | 200 | 推荐模型完成一次有效对话（settled billing + 有输出） |
| `follow_up_success` | 200 | 同一会话连续 2~3 轮追问，每轮 `stop_reason=end_turn`（被 `max_tokens` 截断不算） |
| `expert_success` | 300 | 带 `x-phanthy-expert-id` 的调用 |
| `skill_success` | 300 | 带 `x-phanthy-skill-names` 的调用 |
| `complete_workflow` | 500 | 专家/Skill 会话里完成一次交付 |

注意事项：

- `POST .../onboarding/tasks/<key>/complete` 对 `expert_success`/`skill_success` 返回 200，
  但**不发分**（只把状态置 `in_progress`）；`follow_up_success`/`complete_workflow` 直接
  `400 task_action_not_supported`。积分只认 usage，接口层面骗不出来——这一点本身就是反证。
- 服务端要求用量发生在任务 `started_at` 之后，所以顺序是「先 start（或先前台点开）→ 再真用 → 再 reconcile」。
- 一次跑完的脚本：`python3 scripts/onboard_driver.py`（先 `--status` 看一眼，不花积分）。

实测消耗与收益：约 30 分 credits 换 1500 分积分（约 50:1 的杠杆），全部是真实模型调用。

---

## 3. 账号池：装机身份怎么管

### 3.1 数据模型

服务端把一个「桌面装机」认成 `(accountScope, installationId)`：

- `accountScope` = `/api/oauth/profile` 的 `account.uuid`（也是身份钥匙的 keyring 账号名）；
- `installationId` = `"di_" + base64url(sha256(identity 公钥裸 32B))`，服务端会重算，不符即 `409`。

所以「多一台机器」= **多一把 identity 私钥**，不是编一个 UUID。一个账号一台装机就够用；
装机被风控/冲突时换一把新 identity 即等于换机器。

### 3.2 设备钥匙作用域（关键决策）

| `identity.device_scope` | 语义 | 代价 |
|---|---|---|
| `account`（默认） | 每个账号一套 identity/device/session，等价「N 台机器各登录一个账号」 | 同一出口 IP 会出现 N 台设备 |
| `machine` | 全池共用一把 device 钥匙（真实客户端就是固定 keyring 名 `reward-device-root-v1`），等价「一台机器登录 N 个账号」 | `device_public_key` 相同，服务端一旦按设备维度关联，账号会被串在一起 |

选择建议：**账号来源不同就用 `account`**（默认），只有确实要模拟「一台机器多账号」时才用 `machine`。
两种模式服务端都接受，差别在于反滥用画像。

### 3.3 文件布局

```
auths/
  phanthy-<ts>.json                     # 账号 token（OAuth）
  installation-acct_5f3d2c1b0a9e8d7c6b5a4f3e.json # 该账号的装机身份（identity/device/session seed + 运营状态）
  device-reward-root.json                     # 仅 machine 模式：全池共用的 device seed
  installation-<scope>.bak-<unix>.json        # rotate 后的旧身份备份
```

身份文件字段（`json:"..."`）：

```
accountScope / installationId / identitySeed / deviceSeed / sessionSeed   # 私钥材料，0600
serverInstallationId / edition / platform / version / status / deviceScope # 服务端登记信息
registeredAt / lastClaimDate / lastClaimPoints / streakDay                 # 领取状态
creditsAvailable / onboardingEarned / lastSeenAt / lastError               # 巡检观测
```

所有写盘都是 `临时文件 + rename`，崩溃不会留半截 JSON；新增字段都有 `omitempty`，
旧的单账号身份文件可直接被读入（向后兼容）。

### 3.4 运维动作

```bash
GO=~/.local/go/bin/go

$GO run ./cmd/activity -action status      # 巡检：daily/streak/credits/onboarding/最近错误
$GO run ./cmd/activity -action provision   # 给新账号装一台虚拟桌面（register）
$GO run ./cmd/activity -action claim       # 只领今天该领的（非 eligible 就跳过）
$GO run ./cmd/activity -action reconcile   # 让服务端按真实用量结算 onboarding
$GO run ./cmd/activity -action probe       # 发一条最小真实推理，验证证明链
$GO run ./cmd/activity -action rotate -account <scope|uid>   # 换机（旧身份自动备份）
```

输出：

```
account                    daily     streak credits onboarding note/error
phanthy-<ts>         granted_t 1      3776    1500/1500  probe ok model=phanthy-pro …
```

`-json` 给程序用，`-delay`（默认 2s）在账号之间加抖动。单账号失败只写该身份的 `lastError`，
不影响整池；退出码仅在有非 `register 503` 的失败时为 1。

### 3.5 并发与限流（实测踩到的）

- `register` 在同一装机当日被反复调用后会返回 `503 temporarily_unavailable`（Activity service），
  但身份已在服务端存在，`summary`/`claim` 照常可用。所以 register 必须**降级为可选步骤**；
  CLI 与网关都按「警告不阻断」处理。
- 账号之间串行 + 抖动（默认 2s，服务端配置 `identity.claim_jitter_seconds` 控制定时任务），
  避免同一出口 IP 瞬时打出一串注册/领取。
- 领取幂等由服务端保证：同日同 round 重跑返回 `already_granted`，不会重复加分；
  `roundKey` 是 `summary.daily.server_date`（北京时间口径）。

### 3.6 网关侧

`internal/server` 已改用同一个 `upstream.Pool`（与 CLI 共用 auths 目录与策略），
配置项：

```json
"identity": { "device_scope": "account", "claim_jitter_seconds": 20 }
```

`POST /v1/phanthy/claim?dry=1` 巡检全部账号，`GET /v1/phanthy/activity` 看单账号活动，
定时任务按 `schedule.claim_hours` 领取并回写身份状态。

---

## 4. 仍未闭合 / 边界

- **同一账号多装机**：协议允许（换 identity 即新装机），但同一账号下多装的积分口径未验证，
  当前实现坚持「一账号一装机」，`rotate` 只用于替换而非并存。
- **onboarding 是否可重复领取**：`program_key=desktop_onboarding_2_16_0` 是账号级一次性任务，
  已完成后不再给分；新账号才需要重跑驱动脚本。
- **credits 与 points 是两套账**：`credits.available`（推理额度）随推理消耗，
  `rewards`/`onboarding.earned_points`（积分）是另一套；两者不能互相换算。

---

## 5 模型目录：/v1/models 从哪来，未知名怎么走

### 5.1 上游没有模型清单接口

实测 `GET /v1/models`、`/api/oauth/models` 在 `code.phanthy.com` 都是 **404 HTML**（Next.js 兜底页），
桌面端也不靠接口拿清单：模型 → 上下文窗口这张表是**编译进 sidecar 的常量**（`vx5`），
而「你的套餐允许哪些模型」只在服务端判定，客户端看不到。

所以网关的 `/v1/models` 不是上游代理，而是本地合成：

```
/v1/models =  config.models 的键（别名 + 直通）  ∪  internal/upstream/models.go 的内置目录
```

每条带 `context_length`（内置表命中时）和 `upstream`（别名时给出真正落到哪个上游模型）。
`internal/upstream/models.go` 由 sidecar 的 `vx5` 表生成，51 个 id，覆盖
`phanthy-* / iris / zeus / gaia / apollo / metis / claude-* / deepseek-* / kimi-* / glm-* / minimax-*`。

### 5.2 早期实现的两个问题（已修）

1. **只暴露配置里的 5 个名字**：`config.example.json` 只写了 `phanthy-pro` / `claude-*` / `gpt-4o*`，
   `/v1/models` 就只回这 5 个，看起来像"没有 glm/kimi/deepseek"。
2. **未知模型被静默改写**：`mapModel` 对不在配置里的名字一律回落到 `default_model`，
   于是请求 `glm-5.3` 实际跑的是 `phanthy-pro`，客户端看到的 `model` 字段还是旧模型 —— 不报错、跑错模型，最难查。

现在：别名不变，**未命中的名字原样透传**；要禁止透传就在 config 里开 `upstream.strict_models=true`
（未知模型直接 400 `model_not_allowed`）。

### 5.3 实测：13 个模型全部放行（2026-10-28，真账号，`scripts/probe_models.py`）

每个模型发一次真实 `max_tokens=16` 请求（共约 13 积分），记录上游是否放行与**实际服务模型**：

| # | 请求 model | http | 实际服务模型 | 备注 |
|---|---|---|---|---|
| 1 | `Phanthy-fast` | 200 | `kimi-k2.6` | 上游把 fast 档路由到 kimi，正常 |
| 2 | `Phanthy-pro` | 200 | `Phanthy-pro` | 体验版默认档 |
| 3 | `Phanthy-ultra` | 200 | `Phanthy-ultra` | |
| 4 | `glm-5.3-flash` | 200 | `glm-5.3-flash` | |
| 5 | `glm-5.3` | 200 | `glm-5.3` | 内置表里没有，透传可用 |
| 6 | `glm-5.2` | 200 | `glm-5.2` | |
| 7 | `kimi-k3` | 200 | `Kimi-K3` | 大小写不敏感 |
| 8 | `kimi-k2.7-code` | 200 | `kimi-k2.7-code` | |
| 9 | `deepseek-v4.1-flash-1017` | 200 | `deepseek-v4-1-flash-261017` | 服务端规范化为另一写法 |
| 10 | `Phana-5.5o` | 200 | `Phana-5.5o` | |
| 11 | `Phana-5.5s` | 200 | `Phana-5.5s` | |
| 12 | `Phano-6a` | 200 | `Phano-6a` | |
| 13 | `Phano-6.1s` | 200 | `Phano-6.1s` | |

结论：**13/13 放行，0 被拒**。这 13 个名字已固化进 `config.example.json` 的 `models`（全部直通），
并由 `TestExampleConfigModelsAllListed` 守住 —— 任何人删掉其中一条，`go test` 会红。

两个必须注意的点：

1. **id 必须逐字匹配**。`deepseek-v4.1-flash`（不带 `-1017`）曾返回 `403 model_not_allowed`，
   加上 `-1017` 才 200。模型名不要"猜个近似值"，用网页上原样的字符串。
2. **别名档位会被上游改派**（`Phanthy-fast` → `kimi-k2.6`）。客户端看到的 `model` 字段是改派后的名字，
   不是它请求的那个，这是上游行为，网关原样透传、不做遮盖。

复验命令：

```bash
python3 scripts/probe_models.py                      # 13 个候选
python3 scripts/probe_models.py --from-config        # 按 config.json 的 models 逐个
python3 scripts/probe_models.py --models glm-5.3,kimi-k3 --json data/models_probe.json
```

### 5.4 换套餐/上新模型时怎么改

- 只想让 `/v1/models` 列出来：往 `config.models` 加一条 `"新模型":"新模型"`（直通）；
- 想让某客户端名字落到别的模型：`"gpt-4o":"phanthy-pro"`（别名）；
- 想收紧到白名单：`"strict_models": true`，未列出的名字直接 400。

## 6. 装机身份登记：`desktop_installation_required` 是怎么来的

新号登录后，面板上所有字段都是 0（0 积分、0/0 onboarding、0/2 邀请、从未使用），
账号行里挂着：

```
summary http 401: {"error":{"code":"desktop_installation_required",
  "message":"An active desktop installation for this account is required."}}
```

根因不在 token、也不在网关：**这个账号的装机身份没在服务端登记过**。
`GET/POST /api/oauth/activities/summary` 要求该账号上存在一个 active 的 desktop
installation，否则一律 401；面板拿不到摘要，就用零值兜底显示 —— 所以"全 0"是症状，
不是真的没积分。

判据看身份文件 `auths/installation-<scope>.json`：

| 字段 | 含义 |
| --- | --- |
| `accountScope` / `installationId` / 三把 seed | 本机生成，注册前就有（约 600 字节） |
| `serverInstallationId` | `POST /api/oauth/desktop-installations/register` 返回的 `installation.id` |
| `status` | 同上返回的 `installation.status`（`active`） |

前三个有、后两个没有 = 只在本地生成，服务端从未登记。

修复前的调用链只有一个地方会 register：`claimOne()`（09:00/21:00 的 claim 任务）。
于是「登录 → 第一次 claim」之间这个窗口里，面板必然全零，最长能空一整天。

现在的规则（`internal/server/activities.go`）：

- `summaryFor()` 是所有读摘要路径的唯一入口（看板、登录回显、claim），顺序是
  **续期 token → ensureRegistered → summary**；
- `ensureRegistered()` 只在 `serverInstallationId`/`status` 都缺时才真去登记，
  成功后回写身份文件；失败按 5xx 重试（登录/claim 3 次，看板 1 次），
  并且有 **10 分钟失败冷却**，避免上游 503 期间看板每次轮询都卡几秒；
- summary 仍然返回 `desktop_installation_required` 时（本地记录过期），
  清掉这两个字段**强制重新登记**再重试一次；
- summary 成功但本地没记过 status 的（早期 register 被 503 掉了的情况），
  顺手把 `status=active` 写回 —— 否则每次轮询都会白打一次 register。

面板上这类账号的状态显示为「待登记装机」（黄色），而不是笼统的「摘要失败」。

回归测试：`internal/server/register_test.go`
- `TestSummarySelfRegistersBeforeRead`：假上游在 register 之前对 summary 回 401，
  断言我们自动登记、拿到真实积分、身份文件被回写、第二次不再重复 register；
- `TestSummaryRecoversFromStaleRegistration`：本地写了 `status=active` 但服务端不认，
  断言走"清记录 → 重登记 → 重试"这条自愈路径。

## 7. 只有 thinking 的一轮：网关侧兜底，别把「空正文 + stop」交出去

现象（2026-10-28 报障）：上游 HTTP 200、SSE 正常、耗时 2–4s，但这一轮 assistant 输出
**只有 reasoning，没有正文、也没有 tool_calls，finish_reason=stop**。调用方的
reasoning-only 守卫把它判成 incomplete turn 并重试，一次提问烧 3~4 次额度，
会话记录里留下三行只有 thinking 块的 assistant。

两个成因，都在网关这一侧：

### 7.1 流式路径把 tool_use 整个丢了（真 bug）

上游 `/v1/messages` 的 SSE 里，工具调用是
`content_block_start{content_block.type=tool_use}` + 若干 `input_json_delta`。
旧 `streamChat` 的翻译只认 `delta.text` 与 `delta.thinking`，`content_block_start` /
`input_json_delta` 一律忽略 —— **流式模式下调用方永远收不到 tool_calls**。
于是模型本来要调工具的那一轮，在调用方看来就是「有 thinking、没正文、没工具调用、stop」。

现在（`translateAnthropicStream`）：
- `content_block_start(tool_use)` → 发 `tool_calls[0]{index,id,type,function.name}`；
- `input_json_delta` → 用同一个 `index` 增量追加 `function.arguments`；
- `finish_reason` 按上游 `stop_reason` 映射：`tool_use→tool_calls`、`max_tokens→length`、
  `end_turn`/`stop_sequence`/未知 → `stop`（旧实现恒为 `stop`）。

实测（本机网关，流式 + 一个 read_file 工具）：

```
finish_reason: tool_calls
tool_calls: {"0": {"id":"call_08d4…","name":"read_file","args":"{\"path\": \"/tmp/does-not-exist.txt\"}"}}
```

### 7.2 只有 thinking 的一轮被原样当成「完整回答」

上游这套模型（deepseek 系解析器）会把答案写进 reasoning 里，正文留空。旧实现把这形态
原样交付，调用方只能靠自己的守卫去猜「这是不是没答完」。网关比调用方更清楚这条上游的脾气，
所以默认在网关侧补出来：

`config.upstream.reasoning_only`：

| 取值 | 行为 |
| --- | --- |
| `promote`（默认，空值同义） | 整轮没有正文、没有 tool_use，但有 thinking → 把 thinking 当正文交付 |
| `passthrough` | 原样交付空正文（旧行为），由调用方自己兜底 |

落到三条路径：
- **`/v1/chat/completions` 流式**：thinking 照旧以 `reasoning_content` 逐段下发；收尾时若一个
  正文/工具 delta 都没发过，就先补一个 `content` delta 再发 finish（保证调用方拿得到）；
- **`/v1/chat/completions` 非流式**：正文提升的同时补上 `reasoning_content`
  （旧实现非流式压根不给 reasoning，两种形态不一致）；
- **`/v1/messages`（原生透传）**：非流式会补一个 `text` 块（thinking 块原样保留、`stop_reason`
  不改）；流式逐字节透传，只在 `message_stop` 之前补一组
  `content_block_start / content_block_delta / content_block_stop`。

两种形态都不改写 `usage`、不改写 `stop_reason`，`passthrough` 配置下与旧行为逐字节一致。

实测证据（`internal/server/stream_translate_test.go`，11 个用例）：
- 只有 thinking 的流式一轮 → 正文 = thinking，且补的正文发在 finish 之前；
- `passthrough` → 正文仍为空、`reasoning_content` 保留；
- 工具调用 → 首帧带 `index/id/type/name`，参数按同 index 增量拼接成合法 JSON，`finish_reason=tool_calls`，
  且**不会**顺手把 thinking 提升成正文；
- `stop_reason` 五种取值 → `finish_reason` 映射表；
- 有正文的正常一轮（含 thinking + text + tool_use）三个字段一个不动；
- 真·空回复（无正文、无 thinking、无工具）不凭空造内容，只记日志；
- `/v1/messages` 补块、正常轮不补块、坏 JSON 原样返回。

一个仍然存在的边界：上游若在 `max_tokens` 截断时只给了 thinking，我们是补正文还是留空？
当前按「有 thinking 就补」处理（截断的 thinking 也是模型产出的内容），没有区别对待。

## 8. usage：上游的真实数字在 `message_delta`，别只看 `output_tokens`

现象（2026-10-28 报障：「usage 字段似乎被你扔完了」）：非流式正常（`prompt_tokens=682`、
`cached_tokens=640`、命中 93.8%），**流式却报 `prompt_tokens=8` / `cached_tokens=0` / 命中 0%**，
`/status` 的 `prompt_tokens` 累计和日志里的 `in=`/`hit=` 也跟着偏小。

根因：上游 SSE 在**两个**事件上给 usage，而且两份不一样 ——

| 事件 | 内容 | 能不能当最终值 |
| --- | --- | --- |
| `message_start` | `input_tokens=8`、`cache_read_input_tokens=0` … | **不能**，这是还没算完的早报 |
| `message_delta` | `input_tokens=42`、`cache_read_input_tokens=640`、`output_tokens=N`、`billing_usage.openai_usage.*` | 能，这才是最终账 |

旧 `streamChat` 从 `message_delta` 只取 `output_tokens`，另外三个计数被丢掉，于是流式口径永远
是「8 个 token、零缓存命中」。旧的 `rel15b` 包实测就是这样（见下），并非本轮新引入。

现在：`message_delta` 上四个计数一并更新（各自 >0 才覆盖，防上游只发 `output_tokens` 时把早报
清成 0），并把 `billing_usage.openai_usage.*` 的明细一起带上。

字段映射（流式/非流式共用 `openAIUsage()`，以前两边字段名会漂移）：

```
prompt_tokens          = input_tokens + cache_creation + cache_read      （未命中+新建+命中）
completion_tokens      = output_tokens
total_tokens           = 两者之和
prompt_tokens_details  = { cached_tokens, cache_creation_tokens,
                           cache_creation_input_tokens, cache_read_input_tokens,
                           + 上游明细里本地算不出的键（text_tokens / audio_tokens / image_tokens）}
completion_tokens_details = 上游明细原样（reasoning_tokens / text_tokens / audio_tokens / image_tokens）
```

上游给了但本地也能算的键（`cached_tokens`）以本地计算为准，避免两份数字打架；本地算不出来的
（`reasoning_tokens` 这种「thinking 占了多少输出」）原样透传，不自己编。

实测对比（同一条 `说一句话：你好`，`Phanthy-fast`）：

| | 旧版 rel15b | 现在 |
| --- | --- | --- |
| 流式 `prompt_tokens` | 8 | 682 |
| 流式 `cached_tokens` | 0 | 640 |
| 流式命中率（日志 `hit=`） | 0.0% | 93.8% |
| `completion_tokens_details.reasoning_tokens` | 无 | 222（流式）/ 347（非流式） |
| `/v1/messages` 的 usage | 原样透传（含 `billing_usage`） | 不变，一个字节没动 |

单测：`TestStreamUsageUsesFinalNumbersFromMessageDelta`（早报 8 不能当最终值）、
`TestStreamUsageKeepsEarlyInputWhenDeltaOmitsIt`（delta 只给 output 时不清零）、
`TestStreamUsageCarriesUpstreamBillingDetails`（明细透传）、`TestOpenAIUsageMappingIsShared`
（流式与非流式字段一致）。

## 9. 协议层整体替换为 new-api relaykit + tokenkit（2026-10-28）

§7、§8 那两组手写修复（流式 tool_use、usage 早报/终报、reasoning-only promote）当时是对的，
但手写协议转换的问题是一处一处冒：§7 是流式 tool_use 整块丢失，§8 是 usage 只搬 output_tokens。
手写的质量天花板就在这里 —— 协议转换是个有成熟开源实现的标准活，不该自己写。

协议层现在整体交给 [`new-api`](https://github.com/QuantumNous/new-api) 的两个独立模块：

| 模块 | 干什么 |
| --- | --- |
| `github.com/QuantumNous/new-api/relaykit` v0.3.5 | OpenAI Chat ⇄ Anthropic Messages（含 Responses / Gemini）的请求、非流式响应、流式事件转换；工具调用拼接、usage 合并（`MergeUsageNonZero`）、stop_reason 映射、诊断报告 |
| `github.com/QuantumNous/new-api/tokenkit` | 本地 token 计数：上游不返 usage 时的记账兜底（实测没遇到过，留作保险） |

**许可证随之变更：整个项目转为 AGPL v3**（relaykit/tokenkit 是 new-api 的一部分，AGPL-3.0，
随项目整体开源）。见 LICENSE / NOTICE / THIRD-PARTY-LICENSES.md。构建要求 go 1.26+。

替换掉的代码：`openAIToAnthropic`（请求翻译，~110 行）、`writeOpenAI`（非流式响应，~65 行）、
`translateAnthropicStream` + `streamTr`（流式事件翻译，~140 行）全部删除，换成 `relay.go` 里的
宿主胶水（~300 行，其中一半是注释和 Phanthy 特有逻辑）。

**留在宿主侧、不交给 relaykit 的三件事**（都是 Phanthy 特有或协议无关的）：

1. `normalizeCache`（system 拼字符串、cache_control 断点注入、relocate）——Phanthy 服务端
   只认 system 字符串、缓存断点只能挂在 messages 的块上，这是实测出来的上游行为，不是协议。
   调用顺序不变：relaykit 转出 ClaudeRequest → 序列化成 map → `normalizeCache` → 发上游。
2. reasoning-only promote：relaykit 只做协议转换，不做「把 thinking 当正文」这种针对上游
   模型行为的兜底（它也不该做）。宿主在 `message_delta` 之前判断：整轮没有正文/工具调用、
   只有 thinking → 先补一帧正文再收尾（`upstream.reasoning_only=promote`，默认开）；
   非流式同理（`promoteOpenAIResponse`）。原生 `/v1/messages` 的 `promoteMessagesRaw` /
   `pipeMessagesStream` 不变。
3. usage 形态：relaykit 会在 `message_start` 那一帧就带「早报 usage」并在 `message_delta`
   帧给最终值；OpenAI 流式惯例是只在收尾帧给 usage，宿主把早报那份剥掉、只在 finish 帧下发。

实测（本机网关，真上游）：

- 流式 + read_file 工具：4 帧 tool_calls（首帧带 index/id/type/name，后续同 index 增量拼
  arguments）、`finish_reason=tool_calls`、reasoning_content 逐段下发、usage 在收尾帧
  一次性给（prompt=954 / completion=179 / reasoning_tokens=151，带 billing_usage）；
- 流式普通问答：缓存命中口径正确（prompt=685、cached_tokens=640、hit=93.4%）；
- 非流式：content + reasoning_content + tool_calls 齐备，usage 同口径；
- `/v1/messages` 原生透传一个字节没动。

单测（`internal/server/stream_translate_test.go`）：promote 顺序（正文在 finish 之前）、
passthrough 不提升、流式工具调用三帧拼接、stop_reason 映射、空回复不造内容、
usage 早报剥离 / 终值取 message_delta、`usageFromRelay` 口径折算、原生 `/v1/messages`
补块 —— 15 个用例全绿。
