# 05 · 设备指纹、会话粘性、缓存透传与出站净化

本文全部结论都来自 2026-10-28 对 `code.phanthy.com` 的实测（账号：体验版，模型 `phanthy-pro`）。
推断与实测分开标注：**实测** = 真发请求看到的状态码/字段；**推断** = 从 sidecar 代码读出、未单独验证。

---

## 1. 设备画像（fingerprint）

### 1.1 桌面端到底发什么

从 2.43.0 macOS sidecar 的请求构造（`function DN(...)` / `function JR()` / `Tm3()`）还原：

| 头 | 取值 | 说明 |
| --- | --- | --- |
| `User-Agent` | `claude-cli/2.1.92 (external, cli)` | 模板 `claude-cli/${il} (${USER_TYPE}, ${CLAUDE_CODE_ENTRYPOINT??"cli"})`，`il="2.1.92"` 是内置内核版本 |
| `x-app` | `cli` | 固定 |
| `x-phanthy-client-source` | `desktop` | 进程名，桌面端为 `desktop`，纯 CLI 为 `cli` |
| `x-phanthy-client-version` | `2.43.0` | **桌面端版本**，与 UA 里的 2.1.92 是两套版本号，别混用 |
| `x-request-id` / `x-client-request-id` | uuid v4 | 两个头同值（实测） |
| `X-Claude-Code-Session-Id` | 会话 uuid | 与 `x-phanthy-session` 同值 |
| `x-phanthy-cli-device` | `base64url(JSON)` | 见下 |
| `x-phanthy-expert-id` / `x-phanthy-skill-names` | 仅 onboarding | 进签名串，平时不带 |

`x-phanthy-cli-device` 的结构（`node:os` 的 platform/arch/release + 网卡 IPv4）：

```json
{"version":1,"platform":"darwin","arch":"x64","osRelease":"24.6.0",
 "cliVersion":"2.1.92","ipCandidates":["192.168.1.23"]}
```

服务端用它填 usage 的 `client_os`（形如 `darwin-x64`），缺了就是 `null`。

`metadata.user_id` 是 **JSON 字符串**（不是 `user_xxx_account_yyy` 那种拼串）：

```json
{"device_id":"<32 hex>","account_uuid":"<账号 uuid>","session_id":"<会话 uuid>"}
```

- `device_id` 来自 claude-code 自己持久化的 `userID`（`function jR()`：`crypto.randomBytes(16).toString("hex")`，写进 CLI 配置，之后固定）——**不是** OS 机器码；
- OS 机器码另有一份（`getMachineId()`：macOS `ioreg -rd1 -c IOPlatformExpertDevice` 取 `IOPlatformUUID`，Linux `/etc/machine-id`，Windows 注册表 `MachineGuid`），主要用于本地遥测，未见随请求发出。

### 1.2 网关怎么模拟

`internal/fingerprint`：

- 由**装机身份种子**确定性派生：`platform / arch / osRelease / ipCandidates / deviceId / machineId`；
- 同一份 `installation-<scope>.json` 永远报同一台「机器」，`rotate` 换机才整套换掉；
- `ipCandidates` 按身份生成常见家用网段地址（`192.168.x.y`，偶尔加一个 `10.x.y.z`），避免一池账号共用一个出口 IP 被按设备维度串起来；
- 画像落盘在身份文件的 `profile` 字段，老文件缺字段时按种子补生成。

验证：

```bash
go test ./internal/fingerprint/ -v
```

---

## 2. 会话粘性路由

`internal/session`（`Router`）：

| 行为 | 实现 |
| --- | --- |
| 会话 key 提取 | `X-Phanthy-Session` > `X-Claude-Code-Session-Id` > `metadata.session_id` > `conversation_id/user` > `sha256(system+首条消息)` 前 8 字节 |
| 命中 | 绑定有效 + 账号未冷却 → 直接复用，不再调用选择器 |
| 未命中 | 轮询选一个账号并写绑定 |
| 失效 | 上游 401/403/429 → `Drop(key)`，下个请求换账号；账号级失败 → `DropUID` |
| TTL | 默认 2 小时（`session.sticky_ttl_seconds`），闲置释放 |
| 容量 | 默认 20000 条（`session.max_bindings`），超量按最早过期淘汰 |
| 持久化 | `data/sessions.json`，原子写；网关重启后会话仍落原账号 |

为什么必须粘：证明签名串里带会话 id，服务端把「会话 id + 装机身份 + 访问令牌」绑在一起校验；
追问结算（`follow_up_success`）要求同一会话多轮；前缀缓存也按会话前缀命中。
换账号 = 换机器 = 会话断链，onboarding 与缓存全部重来。

观测：`/status` 的 `sessions` 字段（`bindings/hits/misses/drops`）。

---

## 3. 缓存命中透传

### 3.1 实测数据

同一段会话连续三次，`messages[0].content` 里放一个 ~1800 token 的块并挂 `cache_control: {"type":"ephemeral"}`：

```
call1 in=7  cc=1857  cr=0        # 写缓存
call2 in=7  cc=0     cr=1857     # 命中
call3 in=7  cc=0     cr=1857     # 命中
```

**但命中不稳定**：用完全相同的 body、全新内容（避开旧缓存）、同一会话再跑三轮，会看到
`cc=2637` 连续三次不命中，也会看到 `cc → cr → cc` 交替；加不加 `metadata.user_id`、带不带会话头都会出现两种结果。
判断是**各后端节点本地缓存**（没有全局一致性），命中与否取决于请求落到哪个节点。
所以网关能做、也只该做的三件事是：前缀字节稳定（净化只做确定性改写）、断点位置固定、
每次把真实 `cc/cr` 打日志并在 `/status` 累计——命中率是上游的运气，不能靠网关参数调速。

### 3.2 三个必须处理的上游特性

1. **`system` 只认字符串**（实测）：传 `[{"type":"text","text":"暗号是 BLUE…"}]` 数组时，系统提示词被整段忽略
   （问暗号答「我没有暗号」，`input_tokens` 只剩 401）；传同一个字符串则正常回答。
   → 网关必须把 system 数组拼成字符串（`normalizeCache` 的 flatten）。
2. **`system` 上的 `cache_control` 会随 flatten 消失**，断点要重挂到 `messages` 的最后一块上。
3. **`system` 很大时只写不读**（实测）：system 约 4.5 KB 字符串 + 消息里一个大块，三轮全部 `cc=4224, cr=0`；
   同样内容放进 `messages` 块（system 只留一句短指令）则第二轮起 `cr=1857`。
   → 提供 `cache.relocate_system`（默认关）：超过 `system_inline_bytes`（默认 4096）的 system 会被搬到第一条 user 消息开头，
     system 只留一句短指令。这是唯一绕过该行为的办法，属于有损改写，按需开启。

### 3.3 usage 口径

Anthropic 的 `usage`：

```json
{"input_tokens":7,"cache_creation_input_tokens":1857,"cache_read_input_tokens":0,"output_tokens":8}
```

- `input_tokens` 只算**未命中**部分，总输入 = `input + cache_creation + cache_read`；
- `/v1/messages` 原样透传，另加响应头 `x-p2a-cache-read` / `x-p2a-cache-new`；
- `/v1/chat/completions` 映射为 OpenAI 口径：

```json
"usage": {
  "prompt_tokens": 1864,
  "completion_tokens": 8,
  "total_tokens": 1872,
  "prompt_tokens_details": {
    "cached_tokens": 0,
    "cache_creation_tokens": 1857,
    "cache_read_input_tokens": 0,
    "cache_creation_input_tokens": 1857
  }
}
```

- 每次请求打一行 `[chat] model=… acct=… sticky=… in=… cache_read=… cache_new=… hit=…%`；
- `/status` 的 `cache` 字段累计 `prompt_tokens / cache_read / cache_creation`，可直接算命中率。

---

## 4. 出站净化（prompt 清洗）

`internal/sanitize`。和 workbuddy2api 的净化方向**相反**：Phanthy 桌面端内核本身就是 claude-code，
所以 `You are Claude Code…`、`cc_version=`、`anthropic-beta: claude-code-20250219` 都是真机特征，删了反而不像。
真正要抹的是「中转站/第三方 SDK」的痕迹：

| 类别 | 处理 |
| --- | --- |
| `x-stainless-*`（SDK 版本指纹）、`x-openai-*` | 丢弃 |
| `x-api-key` | 丢弃（桌面端只发 `Authorization: Bearer`，带 x-api-key 像第三方 SDK） |
| `User-Agent: Go-http-client` / `OpenAI/Python` / `node` | 由 fingerprint 覆盖成 `claude-cli/2.1.92 (external, cli)` |
| `anthropic-beta` | 白名单过滤（`oauth-2025-04-20`、`claude-code-20250219`、`interleaved-thinking-*`、`prompt-caching-*` 等），且强制补回 `oauth-2025-04-20` |
| 文本里的 `x-anthropic-billing-header: k=v; k=v;` | 整段剥离 |
| 文本里的裸 `cc_xxx=yyy;` | 剥离（循环，处理多段粘连） |
| 别家身份句（`You are ChatGPT…`） | 改写成中性句 |
| 泄露的 `sk-…` | 打码为 `sk-REDACTED` |

所有改写都是确定性的（同输入同输出），否则会破坏前缀缓存。
正则只吃 `key=value;` 形式，遇到普通文本立即停，避免把后文一起吞掉（有回归测试）。

---

## 5. 配置

```json
{
  "session": { "sticky_ttl_seconds": 7200, "max_bindings": 20000, "state_file": "./data/sessions.json" },
  "cache":   { "disabled": false, "relocate_system": false, "system_inline_bytes": 4096 },
  "sanitize": { "disabled": false, "keep_billing_header": false, "extra_rewrites": [], "extra_strip": [] },
  "models":  { "gpt-4o": "phanthy-pro" },
  "upstream": { "default_model": "phanthy-pro" }
}
```

`models` 未命中的模型名统一回落到 `upstream.default_model`（体验版只有 `phanthy-pro` 可用，
`Iris-1.0`/`Apollo-2.0` 实测 403 `model_not_allowed`）。

---

## 6. 验证

```bash
export PATH=/Users/lwj/.local/go/bin:$PATH
go vet ./... && go test ./...        # fingerprint/session/sanitize/server/upstream 全绿

go build -o /tmp/p2a ./cmd/server
/tmp/p2a -config config.example.json -listen 127.0.0.1:17871 &

# 粘性 + 缓存（同一会话连发三次，第 2 次起应 sticky=true 且 cache_read>0）
curl -s http://127.0.0.1:17871/v1/messages \
  -H 'Content-Type: application/json' -H 'X-Phanthy-Session: demo-1' \
  -d '{"model":"phanthy-pro","max_tokens":32,"system":"You are a relay.",
       "messages":[{"role":"user","content":[
         {"type":"text","text":"<1800 token 的正文>","cache_control":{"type":"ephemeral"}},
         {"type":"text","text":"say ok"}]}]}' -D -

# 状态：sessions/cache 计数
curl -s http://127.0.0.1:17871/status | python3 -m json.tool
```
