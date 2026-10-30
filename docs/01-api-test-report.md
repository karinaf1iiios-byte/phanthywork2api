# API 实测报告（自动化）

> auth: auths/phanthy-<ts>.json  accountId=None  time=2026-10-28 02:57:22 CST

| 结果 | 项 | 详情 |
|---|---|---|
| [PASS | 1.1 access_token 存在 | 前缀=eyJhbGciOiJSUzI1… |
| [PASS | 1.1 refresh_token 存在 | 有 |
| [PASS | 1.1 expiresAt 合理 | 剩余 23 分钟 |
| [PASS | 1.2 refresh_token 刷新 | 新 access 前缀=eyJhbGciOiJSUzI1… 已写回 auths/phanthy-<ts>.json |
| [PASS | 1.3 rewards | HTTP 200 -> {"schema_version": 1, "rewards": [], "next_cursor": null} |
| [PASS | 1.3 referrals/me | HTTP 200 -> {"schema_version": 1, "referral": {"program_status": "active", "eligible_to_invite": false, "eligibility_reason": "desktop_activation_required", "reward_points" |
| [PASS | 1.3 profile | HTTP 200 -> {"account": {"uuid": "acct_5f3d2c1b0a9e8d7c6b5a4f3e", "email": "+86 138****0000", "display_name": "+86 138****0000", "created_at": "2026-10-26T17:46:43.759Z"}, "org |
| [PASS | 1.3 usage/summary | HTTP 200 -> {"schema_version": 1, "plan": {"name": "体验版", "expires_at": "2026-11-25T17:46:43.752Z"}, "usage_by_day_and_model": [], "recent_usage": []} |
| [PASS | 1.3 usage | HTTP 200 -> {"five_hour": {"window": "five_hour", "utilization": 0, "resets_at": "2026-11-25T17:46:43.752Z", "used_credits": 0, "remaining_credits": 2000, "total_credits":  |
| [FAIL | 1.3 usage/requests | HTTP 405（存在，方法不对） |
| [FAIL | 1.3 account/settings | HTTP 404 -> <!DOCTYPE html><html dir="ltr" lang="zh-CN"><head><meta charSet="utf-8"/><meta name="viewport" content="width=device-width, initial-scale=1"/><link rel="stylesh |
| [PASS | 1.3 work/rewards | HTTP 200 -> {"rewards": [], "wallet": {"availablePoints": 0, "reservedPoints": 0, "balancePoints": 0, "accountBalancePoints": 0, "balanceDriftPoints": 0, "futurePoints": 0, |
| [FAIL | 1.3 activities/summary(GET试探) | HTTP 401 -> {"error": {"code": "desktop_installation_required", "message": "A valid signed desktop installation request is required."}} |
| [FAIL | 1.4 create_api_key(phanthy_cli) | HTTP 404（404=官方 prod 固定、用 Bearer 兜底） -> <!DOCTYPE html><html dir="ltr" lang="zh-CN"><head><meta charSet="utf-8"/><meta name="viewport" content="width=device-width, initial-scale=1"/><link rel="stylesheet" href="/_next/st |
| [FAIL | 1.4 create_api_key(claude_cli) | HTTP 404（404=官方 prod 固定、用 Bearer 兜底） -> <!DOCTYPE html><html dir="ltr" lang="zh-CN"><head><meta charSet="utf-8"/><meta name="viewport" content="width=device-width, initial-scale=1"/><link rel="stylesheet" href="/_next/st |
| [FAIL | 1.5 /v1/messages(POST) | HTTP 403 [设备激活门控] -> {"error": {"code": "request_not_authorized", "message": "当前请求不可用，请更新或重新打开客户端后重试。"}} |
| [INFO | 1.6 写操作 | skip: 未加 --allow-write（保持只读） |

