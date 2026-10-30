#!/usr/bin/env python3
"""phanthywork2api 接口实测（拿到 auth 后运行）

用法:
  python3 scripts/test-endpoints.py [--auth auths/phanthy-*.json] [--allow-write]

只读安全操作默认执行；写操作（claim/onboarding/register 等）需 --allow-write 才执行。
输出测试结果到 stdout，并留证到 docs/01-api-test-report.md（--no-report 关闭落盘）。

测试矩阵（对应 docs/00-endpoints.md）：
  1.1 登录令牌自检        : access/refresh/id token 存在性、expiresAt 推算
  1.2 Refresh 刷新        : POST auth.openai.com/oauth/token (grant_type=refresh_token)
  1.3 积分/referrals      : GET code.phanthy.com/api/oauth/rewards|referrals/me|profile（带 Bearer）
  1.4 模型列表            : GET chatgpt.com/backend-api/codex/... 探测端点存在性
  1.5 对话                : POST chatgpt.com/backend-api/codex/responses（最小流式，默认一次）
  1.6 活动（写操作）       : 需 --allow-write；claim 前置依赖 roundKey，另行脚本
"""
import argparse
import glob
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

USER_AGENT = "codex-cli/0.91.0"
CT_JSON = "application/json"


def http(method, url, headers=None, body=None, timeout=30):
    data = body.encode() if isinstance(body, str) else body
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("User-Agent", USER_AGENT)
    req.add_header("Accept", "application/json, text/plain, */*")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read()
            txt = raw.decode("utf-8", "replace")
            try:
                return r.status, json.loads(txt)
            except json.JSONDecodeError:
                return r.status, txt
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace")
        try:
            return e.code, json.loads(raw)
        except json.JSONDecodeError:
            return e.code, raw[:800]
    except Exception as e:  # noqa: BLE001
        return -1, str(e)


def find_auth(arg) -> str:
    if arg:
        return arg
    files = sorted(glob.glob("auths/phanthy-*.json"))
    if not files:
        sys.exit("未找到 auth 文件；请先完成登录（login.py exchange ...）")
    return files[0]


def sec(t):
    return int(t) if isinstance(t, (int, float)) else 0


def load_auth(path):
    with open(path) as f:
        return json.load(f)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--auth", default="", help="auth JSON 路径")
    ap.add_argument("--allow-write", action="store_true", help="允许执行注册/领取等写操作")
    ap.add_argument("--no-report", action="store_true", help="不落盘报告")
    args = ap.parse_args()

    path = find_auth(args.auth)
    doc = load_auth(path)
    a = doc.get("auth", {})
    acct = doc.get("account", {})
    at = a.get("accessToken", "")
    rt = a.get("refreshToken", "")
    lines = []
    def report(tag, ok, detail=""):
        lines.append(f"[{'PASS' if ok else ('INFO' if not ok and detail.startswith('skip') else 'FAIL')}] {tag}  {detail}")
        print(lines[-1])

    print(f"== auth: {path} ==")
    print(f"accountId={acct.get('accountId')} email={acct.get('email')}")

    # 1.1 登录令牌自检
    report("1.1 access_token 存在", bool(at), f"前缀={at[:16]}…" if at else "缺失")
    report("1.1 refresh_token 存在", bool(rt), "有" if rt else "无")
    eta = sec(a.get("expiresAt"))
    if eta:
        remain = eta - int(time.time())
        report("1.1 expiresAt 合理", remain > 0, f"剩余 {remain // 60} 分钟")

    bh = {"Authorization": f"Bearer {at}"}
    beta = {"anthropic-beta": "oauth-2025-04-20"}

    # 1.2 Refresh 刷新（POST code.phanthy.com/oauth/token；会轮换 token 落盘）
    if rt:
        st, tok = http("POST", "https://code.phanthy.com/oauth/token",
                       body=urllib.parse.urlencode({
                           "grant_type": "refresh_token",
                           "refresh_token": rt,
                           "client_id": "phanthy-code-cli",
                       }), headers={"Content-Type": "application/x-www-form-urlencoded", **beta})
        if st == 200 and tok.get("access_token"):
            a["accessToken"] = tok["access_token"]
            if tok.get("refresh_token"):
                a["refreshToken"] = tok["refresh_token"]
            a["expiresAt"] = int(time.time()) + sec(tok.get("expires_in") or 3600)
            with open(path, "w") as f:
                json.dump(doc, f, indent=2)
            report("1.2 refresh_token 刷新", True, f"新 access 前缀={tok['access_token'][:16]}… 已写回 {path}")
        else:
            report("1.2 refresh_token 刷新", False, f"HTTP {st}: {json.dumps(tok, ensure_ascii=False)[:200]}")
    else:
        report("1.2 refresh_token 刷新", False, "skip: 无 refresh_token")

    # 1.3 积分 / referrals / profile / usage（只读；先无签名试，看是否还要求桌面签名四头）
    # 端点来自 sidecar 直读（2026-10-27）：/api/oauth/* 29 个 + /api/work/*（Work 专属）
    for tag, p in [("1.3 rewards", "/api/oauth/rewards"),
                   ("1.3 referrals/me", "/api/oauth/referrals/me"),
                   ("1.3 profile", "/api/oauth/profile"),
                   ("1.3 usage/summary", "/api/oauth/usage/summary"),
                   ("1.3 usage", "/api/oauth/usage"),
                   ("1.3 usage/requests", "/api/oauth/usage/requests"),
                   ("1.3 account/settings", "/api/oauth/account/settings"),
                   ("1.3 work/rewards", "/api/work/rewards?limit=5"),
                   ("1.3 activities/summary(GET试探)", "/api/oauth/activities/summary")]:
        st, body = http("GET", "https://code.phanthy.com" + p, headers=bh, timeout=20)
        snippet = json.dumps(body, ensure_ascii=False)[:160] if not isinstance(body, str) else body[:160]
        if st == 401:
            report(tag, False, f"HTTP 401 -> {snippet}")
        elif st == 405:
            report(tag, False, f"HTTP 405（存在，方法不对）")
        else:
            report(tag, st < 400, f"HTTP {st} -> {snippet}")

    # 1.4 对话前置：create_api_key（phanthy_cli + claude_cli 两路；prod 或 404 用 Bearer 兜底）
    for tag, p in [("1.4 create_api_key(phanthy_cli)", "/api/oauth/phanthy_cli/create_api_key"),
                   ("1.4 create_api_key(claude_cli)", "/api/oauth/claude_cli/create_api_key")]:
        st, body = http("POST", "https://code.phanthy.com" + p,
                        headers={**bh, "Content-Type": CT_JSON, **beta}, timeout=30)
        snippet = json.dumps(body, ensure_ascii=False)[:180] if not isinstance(body, str) else body[:180]
        report(tag, False, f"HTTP {st}（404=官方 prod 固定、用 Bearer 兜底） -> {snippet}")

    # 1.5 对话（Phanthy /v1/messages，Anthropic 格式；小消息一条）。
    # 实测：403 request_not_authorized（设备激活门控），无独立推理域；chatgpt.com 已排除
    if at:
        msg = {
            "model": "claude-sonnet-4-5",
            "max_tokens": 64,
            "messages": [{"role": "user", "content": "ping"}],
        }
        st, body = http("POST", "https://code.phanthy.com/v1/messages",
                        headers={**bh, "Content-Type": CT_JSON, "x-app": "cli",
                                 "User-Agent": "phanthy-code-cli/2.36.1",
                                 "x-phanthy-client-source": "desktop"},
                        body=json.dumps(msg), timeout=90)
        snippet = json.dumps(body, ensure_ascii=False)[:220] if not isinstance(body, str) else body[:220]
        note = "设备激活门控" if st == 403 else ""
        report("1.5 /v1/messages(POST)", st == 200, f"HTTP {st} [{note}] -> {snippet}")
    else:
        report("1.5 /v1/messages(POST)", False, "skip: 无 access_token")

    # 1.6 活动写操作（仅 --allow-write，另行按需）
    if args.allow_write:
        report("1.6 claim 等写操作", False, "skip: 需先完成设备注册自举（register），见 register-device.py")
    else:
        report("1.6 写操作", False, "skip: 未加 --allow-write（保持只读）")

    if not args.no_report:
        os.makedirs("docs", exist_ok=True)
        with open("docs/01-api-test-report.md", "w") as f:
            f.write("# API 实测报告（自动化）\n\n")
            f.write(f"> auth: {path}  accountId={acct.get('accountId')}  time={time.strftime('%Y-%m-%d %H:%M:%S %Z')}\n\n")
            f.write("| 结果 | 项 | 详情 |\n|---|---|---|\n")
            for l in lines:
                m = l[1:].partition("] ")[::2]
            for l in lines:
                head, _, rest = l.partition("] ")
                tag, _, detail = rest.partition("  ")
                f.write(f"| {head} | {tag} | {detail} |\n")
            f.write("\n")
        print(f"\n报告已落盘: docs/01-api-test-report.md（{len(lines)} 行）")


if __name__ == "__main__":
    main()