# data/ — 运行态（本目录内容不入库）

运行时由 `cmd/activity`、`scripts/pool.sh` 生成，含账号池状态与会话粘性表，可能夹带
账号 scope / 邀请信息，所以 `.gitignore` 一律排除。

| 文件 | 内容 |
|---|---|
| `pool_status.json` | 全池最近一次巡检快照（credits / onboarding / referral 档位 / 错误） |
| `invite_candidate.json` | `pool.sh invite` 生成的候选邀请人 + 注册链接 |
| `sessions.json` | 会话→账号粘性表（TTL 2h，最多 20000 条） |
| `run.lock/` | 定时任务互斥目录（`mkdir` + pid，macOS 无 flock） |

删掉即重置；下一次运行会自动重建（账号身份在 `auths/`，不受影响）。
