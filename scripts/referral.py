#!/usr/bin/env python3
"""phanthywork 邀请（referral）全链：查询邀请资格 / 生成邀请码 / 邀请关系列表 / 绑定邀请码

端点来源：PhanthyWork 2.43.0 macOS claude-sidecar 内本地活动代理路由表（route map）：
  "referral-summary"   GET  /api/oauth/referrals/me
  "referral-code"      POST /api/oauth/referrals/code
  "referral-relations" GET  /api/oauth/referrals/relations?<query>
签名与每日签到完全一致（activity canonical v1，换行分隔，upstream 含 query），
所以直接复用 claim_daily.py 的 signed_call。

用法：
  python3 scripts/referral.py --probe                 # 只读：me + relations
  python3 scripts/referral.py --code                  # 生成/取回专属邀请码
  python3 scripts/referral.py --relations --limit 20
  python3 scripts/referral.py --bind ABCD-EFGH-JK23   # 补填邀请码（试运行，看是否 404）
"""
import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import claim_daily as cd


def me(token, ident):
    return cd.signed_call(token, ident, "GET", "/referrals/me", None)


def code(token, ident, body="{}"):
    return cd.signed_call(token, ident, "POST", "/referrals/code", body)


def relations(token, ident, query=""):
    path = "/referrals/relations" + (("?" + query.lstrip("?")) if query else "")
    return cd.signed_call(token, ident, "GET", path, None)


def bind(token, ident, state):
    """用 /r/<code> 页面里的 referral_state 绑定（只有新手机号验证账号能成功）。"""
    body = json.dumps({"state": state}, separators=(",", ":"))
    return cd.signed_call(token, ident, "POST", "/referrals/bind", body)


def state_from_share(url):
    """抓分享页里的 referral_state=rs1.…（注册/登录链接都会带上）。"""
    import re
    import urllib.request
    req = urllib.request.Request(url, headers={"User-Agent": cd.UA})
    html = urllib.request.urlopen(req, timeout=25).read().decode("utf-8", "replace")
    found = re.findall(r"referral_state=(rs1\.[^&\"']+)", html)
    return list(dict.fromkeys(found))


def load(args):
    auth_path = args.auth or sorted(
        (os.path.join(cd.AUTH_DIR, f) for f in os.listdir(cd.AUTH_DIR)
         if f.startswith("phanthy-") and f.endswith(".json")),
        key=os.path.getmtime)[-1]
    doc = cd.load_auth(auth_path)
    token = cd.access_token(doc, auth_path)
    st, profile = cd.http("GET", "/api/oauth/profile", {"Authorization": "Bearer " + token})
    if st != 200:
        sys.exit(f"profile 失败 HTTP {st}: {profile}")
    uid = profile["account"]["uuid"]
    ident_path, ident = cd.load_or_create_identity(uid)
    st, reg = cd.register(token, ident)
    print(f"[register] {st} {json.dumps(reg, ensure_ascii=False)[:200]}")
    return token, ident, uid


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--auth")
    ap.add_argument("--probe", action="store_true", help="me + relations")
    ap.add_argument("--code", action="store_true", help="POST /referrals/code")
    ap.add_argument("--relations", action="store_true")
    ap.add_argument("--limit", default="20")
    ap.add_argument("--bind", help="尝试补填邀请码：传 referral_state（rs1.…）")
    ap.add_argument("--state-url", help="从分享链接抓 referral_state，例如 https://code.phanthy.com/r/<code>")
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args()

    token, ident, uid = load(args)
    out = {"uid": uid}
    todo = []
    if args.probe:
        todo += ["me", "relations"]
    if args.code:
        todo.append("code")
    if args.relations:
        todo.append("relations")
    if args.bind or args.state_url:
        todo.append("bind")
    if not todo:
        todo = ["me", "relations"]

    st = ""
    for step in dict.fromkeys(todo):
        if step == "me":
            st, r = me(token, ident)
        elif step == "code":
            st, r = code(token, ident)
        elif step == "relations":
            st, r = relations(token, ident, "limit=" + str(args.limit))
        else:
            state = args.bind
            if args.state_url:
                states = state_from_share(args.state_url)
                print(f"[state] 页面里找到 {len(states)} 个 referral_state")
                state = state or (states[0] if states else "")
            if not state:
                sys.exit("bind 需要 referral_state：--bind rs1.… 或 --state-url <分享链接>")
            st, r = bind(token, ident, state)
        out[step] = {"status": st, "body": r}
        print(f"[{step}] {st} {json.dumps(r, ensure_ascii=False)[:1200]}")

    if args.json:
        print(json.dumps(out, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
