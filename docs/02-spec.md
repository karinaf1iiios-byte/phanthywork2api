# phanthywork2api 规格说明书（spec v2 · 实测定稿）

> 状态：**定稿 v2**。全部事实来自 docs/00-endpoints.md（逆向 + 2026-10-28 真实 token 两轮实测）与 docs/01-api-test-report.md。
> v1 曾按"ChatGPT/Codex 账号 + chatgpt.com 对话"假设设计，**实测推翻了该假设**：Phanthy 是独立账号体系（`phanthy-code-cli` OAuth），对话走 `code.phanthy.com/v1/messages`（Anthropic 协议），积分走 `/api/oauth/*`，且推理/活动被设备激活门控。
> 目标形态对标 workbuddy2api（Go 1.22+，Docker 部署，登录→网关→定时任务）。

---

## 1. 定位与范围

**做什么**：把用户的 Phanthy 账号变成一台自托管网关：

- OAuth 登录（PKCE，`code.phanthy.com`）→ 持久化 auth（**实测 PASS**）
- 自动 refresh（`/oauth/token` refresh_token，1800s，**实测 2 轮 PASS**）
- 查询积分/套餐/奖励/推荐/用量（`/api/oauth/*` 与 `/api/work/*`，**实测 6 端点 200**）
- OpenAI 兼容对话代理（`/v1/messages` → `code.phanthy.com/v1/messages`，**实测 403 设备激活门控**，见 §5 阻塞）
- 定时活动领取（claim / onboarding，**实测 401 设备激活门控**，见 §6 阻塞）

**不做什么**：不做伪造签名/绕过鉴权/代码注入；领取仅对用户自己账号按官方活动规则执行；不模拟设备注册（该动作需真实桌面端，由用户侧执行）。

## 2. 组件与目录（同 v1，新增 internal/scheduler 已预留）

```
cmd/login/        PKCE 登录（url=打印授权 URL；exchange=回调 code 换 token）
cmd/server/       网关 + 调度主程序
internal/auth/    Auth 凭证（解析/加锁读取/SaveAtomic/NeedsRefresh）
internal/upstream/ HTTP 客户端：token 刷新、签名四头、双写映射、Anthropic 代理
internal/scheduler/ 定时任务（claim_hours / onboarding_hours / keepalive_hours）
internal/server/   代理 handler + admin + metrics
scripts/          login.sh / test-endpoints.py（实测矩阵，含 --allow-write 门）
Dockerfile / docker-compose.yml / config.example.json
```

## 3. 鉴权（实测定稿）

### 3.1 登录（PKCE，实测全链路 PASS）
- 授权 `https://code.phanthy.com/oauth/authorize`；令牌 `https://code.phanthy.com/oauth/token`
- `client_id=phanthy-code-cli`；`scope=org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload`
- PKCE S256；`response_type=code`；回调 `https://code.phanthy.com/oauth/code/success`（页面显示 `code#state`）
- 换 token：`grant_type=authorization_code` + `code_verifier`（实测可省 `state`/`redirect_uri`；`code#state` 需剥离 `#` 后使用）
- 落盘 auth JSON：`{"auth":{accessToken,refreshToken,expiresAt,tokenType},"account":{uid,nickname}}`（实测格式）

### 3.2 刷新（实测 PASS，可循环轮换）
- `POST https://code.phanthy.com/oauth/token`，`grant_type=refresh_token` + `client_id=phanthy-code-cli`
- 响应 `expires_in=1800`；阈值：剩余 ≤ 5 分钟触发；刷新后原子写回（tmp+rename）
- 并发：`mu sync.Mutex`，出站取值一律加锁读

### 3.3 设备签名（活动/推理门控，A 级已还原、实测未放行）
```
x-desktop-installation-id / x-desktop-timestamp(ms) / x-desktop-nonce(b64url(18B)) / x-desktop-signature
签名串: v1 <METHOD> <upstreamPath> <ts> <nonce> <b64url(SHA256(body))> <idempotencyKey>
算法: Ed25519（PKCS8）；body 仅 POST；写操作带 idempotency-key
```
实测结论：积分类接口**纯 Bearer 即放行**；活动/推理额外要求 `desktop_installation_required`/`request_not_authorized`——**需先在真实桌面端完成设备注册**（`/api/oauth/desktop-installations/register`，会生成安装 ID + 设备私钥）。沙箱无法模拟，网关预留签名中间件但默认关闭（`sign.enabled=false`）。

## 4. 上游白名单（实测定稿）

| 远端 | 方法 | 实测 | gateway 入口 |
|---|---|---|---|
| `/api/oauth/profile` | GET | **200**（uuid/email 脱敏/display_name） | `GET /v1/phanthy/profile` |
| `/api/oauth/usage/summary` | GET | **200**（plan=体验版、expires_at） | `GET /v1/phanthy/credits` |
| `/api/oauth/usage` | GET | **200**（five_hour 窗口 2000 积分） | `GET /v1/phanthy/usage` |
| `/api/oauth/rewards` | GET | **200**（空列表） | `GET /v1/phanthy/rewards` |
| `/api/oauth/referrals/me` | GET | **200**（program_status=active；desktop_activation_required） | `GET /v1/phanthy/credits` |
| `/api/work/rewards?limit=5` | GET | **200**（wallet 点数全零） | `GET /v1/phanthy/work-rewards` |
| `/api/oauth/usage/requests` | GET | 405（存在，方法待定） | —（暂不暴露） |
| `/api/oauth/activities/summary` | GET | **401 门控** | `GET /v1/phanthy/activity`（设备激活后可用） |
| `/api/oauth/activities/daily-login/claim` | POST | 405=仅 POST | 调度器自动（设备激活后） |
| `/api/oauth/desktop-installations/register` | POST | 405=仅 POST（自举起点） | 用户侧执行 |
| `/api/oauth/account/settings` | GET | **404（不存在）** | 删除 |
| `/api/oauth/{phanthy,claude}_cli/create_api_key` | POST | 404（非网关路径） | 删除 |
| `chatgpt.com/backend-api/codex/responses` | — | **403（排除）** | 删除 |

## 5. 对话代理（实测阻塞标注）

- 入站：`POST /v1/messages`（Anthropic Messages 格式）
- 出站：`POST https://code.phanthy.com/v1/messages`，Anthropic 协议 + SSE；头 `Authorization: Bearer <phanthy token>`、`anthropic-version: 2023-06-01`、`x-phanthy-client-source: desktop`、UA `phanthy-code-cli/<ver>`
- **实测**：带全头仍 403 `request_not_authorized`——推理需设备激活（与活动同源门控）。模型集合由激活后 provider 决定，`/v1/models` 不存在。
- 实现保留协议与映射（SSE → content_block_delta/stop_reason），`chat_available=false` 时返回明确 503/403 错误体；设备激活后仅需放开 `sign.enabled` 与模型名回填。

## 6. 定时任务（调度器）

```json
"schedule": {
  "claim_hours": [10], "onboarding_hours": [11], "keepalive_hours": [22],
  "claim_enabled": true, "onboarding_enabled": true, "keepalive_enabled": true
}
```
- **keepalive：可用（refresh 实测通过）**——按阈值刷新并原子写回。
- claim/onboarding：**设备激活前置**（401 门控），实现保留幂等键（`daily:<installationId>:<roundKey>`）与守卫，激活后由用户侧开 `claim_enabled`。
- 全部任务取值走加锁读（workbuddy2api race 教训）。

## 7. 配置模板（config.example.json，实测定稿）

```json
{
  "listen": ":7863",
  "api_key": "test_key",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "schedule": { "claim_hours":[10], "onboarding_hours":[11], "keepalive_hours":[22],
                "claim_enabled": false, "onboarding_enabled": false, "keepalive_enabled": true },
  "upstream": {
    "timeout_seconds": 120,
    "user_agent": "phanthy-code-cli/2.36.1",
    "phanthy_base": "https://code.phanthy.com",
    "sign": { "enabled": false }
  },
  "redis": { "url": "" }
}
```

## 8. Docker（同 v1，不依赖沙箱）

- Dockerfile：`golang:1.25-alpine`（`CGO_ENABLED=0 go build`）→ `alpine`；非 root
- compose：服务 + 可选 redis；卷挂 `./auths`、`./config.json`；健康检查 `GET /v1/health`
- 沙箱无 docker：`go build`/`go vet` 验证，镜像由用户侧构建

## 9. 里程碑（loop 顺序，实测后修正）

1. **auth 包**（解析/锁/写回/刷新判定）→ 单测 ✅（已建）
2. **upstream 包**（oauth 客户端 + 签名四头 + Anthropic 代理）→ 单测（Ed25519 RFC8032 向量）✅（已建）
3. **server 包**（/v1/health、/v1/phanthy/* 代理、/v1/messages）→ handler 单测 ✅（骨架已建）
4. **scheduler 包**（keepalive 实测可用；claim 门控前置）→ 时间驱动单测
5. cmd/login + cmd/server + wiring ✅（已建，冒烟通过）
6. Dockerfile + compose + config.example ✅（已建）
7. **设备激活（用户侧，一次性）**：真实桌面端登录 → `/desktop-installations/register` → 导出安装 ID + 设备私钥 → 网关 `sign.enabled=true` + `claim_enabled=true`
8. **端到端验收**：test-endpoints.py 全绿（含活动/对话 PASS）→ 网关跑通对话

## 10. 验收断言（客观化）

- `go vet ./...` 0 错误；`go build ./...` 通过
- Ed25519 签名单测含 RFC 8032 TEST1 逐字节断言 + 负例
- `curl /v1/health` 200；`/v1/phanthy/credits` 返回实测字段（remaining_credits/total_credits）
- refresh 两次触发后凭证文件 expiresAt 更新（脚本断言）
- 活动/对话在门控开启前返回 503 `device_activation_required`（诚实降级）
- `docker compose config` 校验通过（用户侧）
