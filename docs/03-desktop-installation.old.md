# 03 · 桌面安装自举（desktop-installation）逆向结论

> 状态：**已定界（hard boundary）**。协议 100% 还原，服务端门槛无法用纯客户端手段越过。
> 证据日期：2026-10-28，账号 `phanthy-<ts>`。

## 1. 三把钥匙与职责（已确认）

| 钥匙 | 生成方 | 签名域标签 | 用在哪 |
|---|---|---|---|
| identity（身份） | Rust `desktop_installation.rs`，首次调用时生成，存 Windows 凭据管理器 | `daily-activity-ed25519-v4` | 所有 `x-desktop-signature` 请求签名；`register` body 的 `public_key` |
| device（设备） | 同上 | `desktop-device-v1` | `device_signature`：证明"本设备属于该 identity" |
| inference / session | **sidecar 每次启动新生成**（`yF3("ed25519")`） | `reward-device-root-v1` | `register` body 的 `inference_public_key`；`x-phanthy-proof-*` |

`daily-activity/session-key` 是 **sidecar 本地端点**（127.0.0.1:3456），远端 `/api/daily-activity/*` 全部 404（Next.js HTML）。远端只认 `/api/oauth/*`。

## 2. 两条 canonical string（逐字取自 `App-IdIFSKLQ.js`）

```
binding  = "desktop-device-v1 {accountScope} {installationId} {identityPublicKey} {sessionPublicKey} {ts} {nonce}"
active   = "v1 {METHOD} {upstreamPath} {ts} {nonce} {base64url(SHA256(body))} {idempotencyKey}"
```

- `accountScope` = `/api/oauth/profile` 的 `account.uuid`（= JWT `sub`）
- `ts` = `String(Date.now())`（毫秒）
- `nonce` = base64url(18 随机字节)，24 字符无 padding
- 公钥编码：SPKI DER 前缀 `302a300506032b6570032100` + 32 字节裸公钥，base64url 无 padding

## 3. 服务端门槛矩阵（实测，30+ 组合）

`POST /api/oauth/desktop-installations/register`

| 变量 | 取值 | 结果 |
|---|---|---|
| 域标签前缀/后缀 | 无 / `L ` / `L` / `L\n` / payload+`L` | **全部同错** |
| device_signature 签名者 | device 私钥 / identity 私钥 | 同错 |
| device 签名域前缀 | 无 / `L ` / `L` | 同错 |
| 公钥编码 | std / std-nopad / url / url-nopad | 同错 |
| `x-desktop-installation-id` | uuid4 / uuid4 大写 / JWT `sid` / JWT `sub` / `inst_*` | 同错 |
| 时间戳 | 当前 / 偏移 -2h | 同错 |
| 附加头 | 12 个 sidecar `x-phanthy-*` 全套、`x-desktop-edition/platform/version` | 同错 |

**唯一能改变错误码的因素是签名编码**：

- 标准 base64（88 字符，含 padding）→ `401 desktop_installation_required`
- url-safe 无 padding（86 字符）→ `401 invalid_desktop_signature`

⇒ 服务端**先做 base64 长度/解码校验**，通过后只做"该 installationId 是否存在"的查表，**完全不校验签名、不解析 body 公钥**。

## 4. 端点状态机（实测）

| 端点 | 无 Bearer | 仅 Bearer | Bearer + idem + 签名头 |
|---|---|---|---|
| `register` | `missing_login` | `idempotency_key_required` | `desktop_installation_required` |
| `activities/summary` | `missing_login` | `desktop_installation_required` | 同 |
| `activities/daily-login/claim` | — | `idempotency_key_required` | `desktop_installation_required` |

`OPTIONS register` → `allow: OPTIONS, POST`；`PUT` → 405；`GET` → 405；`/api/oauth/desktop-installations`（无 `/register`）→ 404。
`api.phanthy.com/api/oauth/*` → 404（Go 风格），该域只有 `/healthz`；业务全在 `code.phanthy.com`（Kong 2.5.1 网关后）。

`GET /api/oauth/referrals/me` → `eligibility_reason: "desktop_activation_required"`，与上述门控同源。

## 5. 已确认可用的接口（Bearer 即可）

| 接口 | 结果 |
|---|---|
| `GET /api/oauth/profile` | 200，`account.uuid / organization` |
| `GET /api/oauth/usage` | 200，`five_hour{used_credits,remaining_credits,total_credits,...}` |
| `GET /api/oauth/rewards` | 200，`rewards: []` |
| `GET /api/oauth/referrals/me` | 200，`program_status: active` |
| `POST /oauth/token`（refresh） | 200，`expires_in=1800` |

门控后（需设备激活）的接口：`activities/summary`、`activities/daily-login/claim`、`referrals/code|relations`、`/v1/messages`（推理，403 `request_not_authorized`）。

## 6. 为什么无法在服务器/沙箱内自举

1. **没有 Linux/ macOS 版 PhanthyWork**。`/api/download/client?platform=` 只发布 PhanthyCode（linux-x86_64 / windows-x64 / macos-arm64），无 Work 变体；`releases/latest/PhanthyWork-*` 全部 404。
2. Linux 版 PhanthyCode 二进制内 **完全没有** `desktop_installation` / `daily-activity` 符号（`strings` 零命中）——该能力仅存在于 Windows `PhanthyWork.exe`。
3. Windows 版是 Tauri 2.10.3，**必须 WebView2**；Wine 无 WebView2，`setup()` 前即退出，Rust 侧命令无法被调用。
4. 私钥走 `CredWrite`（Windows 凭据管理器），不在明文文件；无密码派生路径。

⇒ 结论：**register 需要一条由真实客户端创建的安装记录，服务端不提供任何"凭公钥自助注册"的入口。** 这不是签名实现问题，是产品侧的服务端状态机。

## 7. 可行的取数路径（需真机一次）

二选一，任选其一即可永久解锁（拿到后本机可完全离线复现）：

**A. 导出凭据管理器条目**
```powershell
# Windows 上登录 PhanthyWork 并打开"每日活动"页一次，然后：
cmdkey /list | findstr -i phanthy
# 或用 PowerShell 读取 generic credential 的 blob
```
条目名可从 `PhanthyWork.exe` 的 `desktop_installation.rs` 附近字符串定位；blob 即 identity/device 私钥（PKCS8）。

**B. 便携模式（portable）配置目录**
二进制含 `PHANTHY_WORK_EXISTING_INSTALLATION`、`determine_startup_portable_dir`、`dir_has_portable_data`。
以 `--portable` 启动后整个配置目录可整包拷出，供 `PHANTHY_WORK_EXISTING_INSTALLATION` 指回复用。

拿到后需要落盘的最小集合：

```json
{
  "installationId": "uuid",
  "accountScope": "acct_5f3d2c1b0a9e8d7c6b5a4f3e",
  "identitySeed": "base64(32B)",
  "deviceSeed":   "base64(32B)"
}
```

## 8. 对 Go 项目的处理方式

`internal/upstream/sign.go` 里实现完整两条 canonical string + Ed25519 签名；
`installation` 作为可插拔模块：
- 有 `installation.json` → 走 `register → summary → claim` 全链；
- 无 → `activities/*` 返回明确错误 `desktop_installation_required`（与上游一致），其余接口正常。

这样一旦拿到真机导出，**无需改代码**即可解锁积分领取。
