# workbuddy2api 参考架构提炼（供 phanthywork2api 设计复用）

> 来源：`workbuddy2api-master`（用户提供，Go 1.22.5，仅 redis 一个第三方依赖）。
> 用途：phanthywork2api 的 Go 项目结构/鉴权/调度/部署模式直接对标。

---

## 1. 目录结构

```
cmd/login/           登录：url → 打印授权 URL（state 落盘）；poll → 读 state 轮询换 token
cmd/server/          网关主程序：config 装载 + wiring（服务/调度/会话装配）
cmd/activity,credit,acct,signin,stats,trial/   运维命令（签到/积分/账号/info 等）
internal/auth/       Auth 凭证模型：双形态解析 / 加锁读取 / 原子写回 / realm 判定
internal/config/     schedule_hours 定时配置
internal/upstream/   上游 HTTP 客户端：refresh、请求头构造、模型目录、提示词目录
internal/scheduler/  定时任务：checkin/travel/activity/keepalive/school/cat
internal/pool/       在途并发控制（max_in_flight）、熔断、冷却、降级
internal/server/     HTTP 服务：handler/admin/logging/metrics/wafip/backoff
internal/session/    session 粘滞（redis 可选）
internal/redisstore/ redis 可选存储（session/gain 持久化）
scripts/ *.sh        登录/签到/状态快捷脚本
Dockerfile          多阶段构建（golang → alpine 或 slim）
docker-compose.yml  网关 + 可选 redis + auths 挂载
config.example.json 全量配置模板
```

## 2. 鉴权模型（Auth 双形态 + 原子刷新）

- 磁盘形态两种：嵌套形 `{"auth":{accessToken,refreshToken,expiresAt,domain},"account":{uid,enterpriseId,nickname}}`；扁平形直接平铺。
- 并发安全：`mu sync.Mutex` 挂在 Auth 上；出站取 token 一律 `AccessTokenValue()` 加锁读；刷新在锁内改字段并 `SaveAtomic()`（tmp+rename）。
- `NeedsRefresh(within)`：`ExpiresAt<=0 → true`；`now+within>=ExpiresAt → true`。调度 keepalive 用它决定何时刷新。
- realm（cn/global）双上游：base/Origin/Referer 随 realm 切换；`isGlobalDomain` 按 domain 后缀判定。

## 3. 登录（设备流 CLI）

```
login url  → POST {upstream}/v2/plugin/auth/state?platform=CLI
             → 响应 {state, authUrl}；state 落 /tmp/<app>-login-state.json；stdout 打印 authUrl
login poll → GET  {upstream}/v2/plugin/auth/token?state=<state>（一次）
             → 成功再 GET /v2/plugin/login/account?state=<state> 拿 uid/nickname
             → stdout 打印完整 token+account JSON（用户复制落盘）
```
- 无 PKCE（服务端签发 state）；`Origin`/`Referer` 头必须带正确域；UA `CLI/2.63.2 CodeBuddy/2.63.2`。
- 刷新：`POST /v2/plugin/auth/token/refresh`。

## 4. 上游客户端与信封

- 信封 `{code,msg,data}`：`code!=0 → error`（doJSON 家族统一解析）。
- 请求头构造分 Chat 头与 Billing 头两个族（`ChatHeaders`/`BillingHeaders`），Bearer 取 `AccessTokenValue()`。
- `RefreshToken`：① 锁外读 de字段判断过期 ② 锁内校验快照一致后原子写回（防并发双刷新）③ SaveAtomic。
- 模型的全局池/目录在 upstream 侧缓存（global_models / model_catalog / effort_catalog）。

## 5. 调度器（定时任务 = 活动领取）

`config.schedule`：
```json
{ "checkin_hours":[9,21], "travel_hours":[9,21], "activity_hours":[10],
  "keepalive_hours":[22], "school_hours":[12], "cat_hours":[1],
  "checkin_enabled":true, ... }
```
- scheduler 每小时窗口触发；keepalive 对每个非禁用账号 `NeedsRefresh(30m) → RefreshToken`。
- 每个任务前置守卫 **必须** 走加锁取值的 `RefreshTokenValue()`（race 实证，见源码注释）。

## 6. 服务端

- `cmd/server` 装配：config → auth.LoadDir(auths) → client/upstream → scheduler → http server。
- server 暴露 OpenAI/Claude 兼容代理 + 管理端点（admin.go：池/冷却查看与手动禁用）+ metrics。
- pool：`max_in_flight` 并发闸、`breaker_threshold` 熔断、`cooldown` 冷却、`degrade` 降级、`idle_weight` 空闲权重。

## 7. Docker 部署（目标形态）

- Dockerfile：多阶段（`golang:1.22-alpine build → alpine runtime`），产物静态二进制。
- docker-compose.yml：`server` 服务 + `redis`（可选）+ 卷挂载 `auths`（0o600）+ `config.json`；`restart: unless-stopped`。
- 运维：login.sh（登录）→ 把 auth JSON 放进 auths/ → `docker compose up -d` → 定时任务自动跑。

---

## 8. 对 phanthywork2api 的可复用映射

| workbuddy2api | phanthywork2api |
|---|---|
| login url/poll（服务端签发 state） | **OpenAI Codex OAuth PKCE**（agent 生成 URL → 用户授权 → 回传 code 交换；另有本地回调模式） |
| auth 双形态 + SaveAtomic + NeedsRefresh | 同构复用；token 字段换 `{accessToken,refreshToken,idToken,expiresAt}` |
| refresh `POST /v2/plugin/auth/token/refresh` | `POST https://auth.openai.com/oauth/token`（grant_type=refresh_token，阈值 5 分钟） |
| chat/billing 请求头族 | 请求头 = 桌面签名四头 + Bearer（Ed25519 canonical string，见 docs/00-endpoints.md §2） |
| 信封 {code,msg,data} | 远端 401 错误信封 `{error:{code,message}}` |
| scheduler checkin/activity/travel | scheduler：每日登录 claim（roundKey 驱动）+ onboarding + 通知回执 |
| server OpenAI 兼容代理 | 代理 `/v1/messages`→`chatgpt.com/backend-api/codex/responses`（流式 SSE 转换） |
| config.schedule | 同构（claim_hours / onboarding_hours / keepalive_hours） |
| Dockerfile + compose | 同构（auths 卷 + redis 可选） |