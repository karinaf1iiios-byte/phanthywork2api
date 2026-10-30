#!/usr/bin/env python3
"""phanthywork 登录（PhanthyCode CLI OAuth，两步模式）

实测依据（用户提供参考 phanthycode2api，2026-10-27 确认）：
  授权:  GET  https://code.phanthy.com/oauth/authorize
         client_id=phanthy-code-cli  scope=user:inference user:profile user:sessions:claude_code
         PKCE S256  redirect_uri=https://code.phanthy.com/oauth/code/success
  换码:  用户授权后跳 code.phanthy.com/oauth/code/success，页面显示 code（复制回贴）
  换token: POST https://code.phanthy.com/oauth/token
          grant_type=authorization_code code=<code> code_verifier=<v> redirect_uri=... client_id=...
          头 anthropic-beta: oauth-2025-04-20
  refresh: POST /oauth/token   grant_type=refresh_token refresh_token=<rt> client_id=...

用法:
  python3 scripts/login.py url            # 生成授权链接（verifier 保存本地）
  python3 scripts/login.py exchange --code <code>   # 换 token，落盘 auths/phanthy-<ts>.json
"""
import argparse
import base64
import hashlib
import json
import os
import secrets
import sys
import time
import urllib.parse
import urllib.request

BASE = "https://code.phanthy.com"
CLIENT_ID = "phanthy-code-cli"
REDIRECT = "https://code.phanthy.com/oauth/code/success"
# 官方 sidecar 的 scope 全集（PhanthyWork 品牌表 oauthClientId 同为 phanthy-code-cli）
SCOPE = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
BETA = "oauth-2025-04-20"
DEFAULT_EXPIRES = 1800  # 官方 sidecar 默认 expires_in=1800
STATE_FILE = os.path.join(os.path.dirname(__file__), "..", "auths", ".login-state.json")
UA = "phanthy-oauth/0.1"


def b64url(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def random_b64url(n: int) -> str:
    return b64url(secrets.token_bytes(n))


def state_load():
    with open(STATE_FILE) as f:
        return json.load(f)


def state_save(d):
    os.makedirs(os.path.dirname(STATE_FILE), exist_ok=True)
    with open(STATE_FILE, "w") as f:
        json.dump(d, f, indent=2)
        f.flush()
        os.fsync(f.fileno())


def form_post(url, form, extra_headers=None, timeout=30):
    data = urllib.parse.urlencode(form).encode()
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/x-www-form-urlencoded")
    req.add_header("Accept", "application/json")
    req.add_header("User-Agent", UA)
    for k, v in (extra_headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace")
        try:
            return e.code, json.loads(raw)
        except json.JSONDecodeError:
            return e.code, {"raw": raw[:400]}


def step_url():
    verifier = random_b64url(48)
    challenge = b64url(hashlib.sha256(verifier.encode()).digest())
    q = urllib.parse.urlencode({
        "client_id": CLIENT_ID,
        "response_type": "code",
        "redirect_uri": REDIRECT,
        "scope": SCOPE,
        "code_challenge": challenge,
        "code_challenge_method": "S256",
        "state": "p2a-login",
    })
    url = BASE + "/oauth/authorize?" + q
    state_save({"verifier": verifier, "created_at": int(time.time())})
    print("授权链接（请在浏览器打开，用 PhanthyCode 账号登录授权）：\n")
    print("  " + url + "\n")
    print("授权成功后页面会跳转到 code.phanthy.com/oauth/code/success")
    print("页面会显示授权码（code），把它复制粘贴回来：")
    print("  python3 scripts/login.py exchange --code <粘贴的code>")
    print("\nverifier 已保存:", STATE_FILE)


def step_exchange(code):
    st = state_load()
    verifier = st.get("verifier")
    if not verifier:
        sys.exit("未找到 verifier，请先运行: python3 scripts/login.py url")
    code = code.strip()
    if code.startswith("http"):
        code = urllib.parse.parse_qs(urllib.parse.urlparse(code).query).get("code", [""])[0]
    elif "code=" in code:
        code = code.split("code=", 1)[1].split("&")[0]
    code = code.strip()
    if not code:
        sys.exit("code 为空")
    st_code, tr = form_post(BASE + "/oauth/token", {
        "grant_type": "authorization_code",
        "code": code,
        "code_verifier": verifier,
        "redirect_uri": REDIRECT,
        "client_id": CLIENT_ID,
    }, {"anthropic-beta": BETA})
    if st_code != 200 or not tr.get("access_token"):
        sys.exit(f"token 交换失败 HTTP {st_code}: {json.dumps(tr, ensure_ascii=False)[:300]}\n若 invalid_grant：code 已过期，重新运行 url 步骤")
    doc = {
        "auth": {
            "accessToken": tr["access_token"],
            "refreshToken": tr.get("refresh_token", ""),
            "idToken": tr.get("id_token", ""),
            "expiresAt": int(time.time()) + int(tr.get("expires_in", DEFAULT_EXPIRES)),
            "tokenType": tr.get("token_type", "Bearer"),
        },
        "account": {"uid": f"phanthy-{int(time.time())}", "nickname": ""},
    }
    out = os.path.join(os.path.dirname(STATE_FILE), f"phanthy-{int(time.time())}.json")
    with open(out, "w") as f:
        json.dump(doc, f, indent=2)
        f.flush()
    os.remove(STATE_FILE)
    print("登录成功！凭据已保存:", out)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("step", choices=["url", "exchange"])
    ap.add_argument("--code", default="", help="授权码或完整跳转 URL")
    args = ap.parse_args()
    if args.step == "url":
        step_url()
    else:
        step_exchange(args.code)


if __name__ == "__main__":
    main()