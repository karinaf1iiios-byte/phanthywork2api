# auths/ — 凭据目录（本目录内容不入库）

这个目录在仓库里是**空的**：账号 token 与装机私钥只存在于你自己的机器上，
`.gitignore` 已经把 `auths/*.json` 全部排除。发布出去的 zip 里也没有任何真实凭据。

## 这里会有什么

| 文件 | 内容 | 来源 |
|---|---|---|
| `phanthy-<ts>.json` | OAuth 账号 token（`account` + `auth`：access/refresh/expires） | `go run ./cmd/login` 或 `scripts/login.py` |
| `installation-<accountScope>.json` | 这台机器的装机身份：三把 Ed25519 seed + 运营状态 | 首次 `register` 时自动生成 |

## installation 文件字段

```jsonc
{
  "accountScope": "acct_…",          // 账号 uuid，来自 /api/oauth/profile
  "installationId": "di_…",          // = "di_" + b64url(sha256(identity 公钥裸 32B))
  "identitySeed": "…",               // Ed25519 seed（b64url）——活动签名用
  "deviceSeed": "…",                 // 设备钥匙：签 device_signature
  "sessionSeed": "…",                // 会话钥匙：签 desktop-inference-v2 证明
  "identityPublicKey": "…",          // 三者都是 SPKI DER 的 b64url
  "devicePublicKey": "…",
  "sessionPublicKey": "…",
  "serverInstallationId": "…",       // 服务端返回的装机 id，register 后回填
  "edition": "phanthy_work",
  "platform": "macos",
  "version": "2.43.0",
  "status": "active",
  "deviceScope": "account",          // account=一账号一装机的常规形态
  "registeredAt": "…",
  "lastClaimDate": "…",              // 幂等：同日重跑返回 already_granted
  "lastClaimPoints": 300,
  "streakDay": 2,
  "creditsAvailable": 0,             // 最近一次 summary 观测值
  "lastSeenAt": "…",
  "referralInviteCode": "…",         // 邀请码（人读 ABCD-EFGH-JK23 形态）
  "referralShareUrl": "…",           // /r/<code>
  "referralNextPoints": 5000,
  "referralEligible": true,
  "referralCheckedAt": "…",
  "onboardingEarned": 0,             // 0..1500
  "onboardingMax": 1500,
  "onboardingPending": ["…"],        // 未完成的 task_key
  "onboardingCheckedAt": 0
}
```

## 处理原则

- **seed 就是私钥**：三把 seed 任意一把泄露 = 这台机器可被冒用。文件权限 0600，别进 git、别贴日志。
- 日志与摘要里只允许出现 `[REDACTED]`，`scripts/*.py` 与 `cmd/*` 都不会打印 seed / token / 邀请码。
- 想换机：`go run ./cmd/activity -action rotate -account <accountScope>`，旧身份自动备份。
- 想清空：直接删 `auths/` 与 `data/` 两个目录，下次运行会重新注册（服务端 `desktop_installation_conflict`
  会要求沿用原装机身份，所以删之前先确认要放弃这台机器的身份）。
