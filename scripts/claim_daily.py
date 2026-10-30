#!/usr/bin/env python3
"""phanthywork daily-activity 全链（自举 -> summary -> claim -> rewards）

与 scripts/claim.py 的区别：不再等待真机桌面端。
实测（2026-10-28，账号 phanthy-<ts>）已跑通 register 200，链路全部本地可复现。

协议要点（全部来自 PhanthyWork 2.43.0 macOS 二进制 + App-*.js 直读/实测）：
  * 头部 installation-id = "di_" + base64url(sha256(identity 公钥裸 32B))，服务端会校验这个推导（错了 → 409）
  * canonical string 分隔符是换行 "\\n"，不是空格（空格 → 401）
  * register 的 device_signature 必须用「设备钥匙」签（keyring 固定名 reward-device-root-v1），
    用 identity 私钥签 → 401；request 签名才用 identity 私钥
  * installation 的真实 id 是服务端生成的 cuid（响应里的 installation.id），头部 id 与它无关
  * claim 的 roundKey 实为 summary.daily.server_date
"""
import argparse
import base64
import hashlib
import json
import os
import secrets
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from ed25519_min import public_key, sign

BASE = os.environ.get("P2A_BASE", "https://code.phanthy.com")
UA = "phanthy-code-cli/2.43.0"
EDITION = "phanthy_work"
VERSION = "2.43.0"
SPKI = bytes.fromhex("302a300506032b6570032100")
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
AUTH_DIR = os.path.join(ROOT, "auths")


def b64url(b):
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def unb64url(s):
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))


def spki(pub32):
    return b64url(SPKI + pub32)


def keypair(seed):
    return seed, public_key(seed)


# ---------------------------------------------------------------- http
def http(method, path, headers=None, body=None, timeout=25):
    req = urllib.request.Request(BASE + path,
                                 data=body.encode() if body is not None and method == "POST" else None,
                                 method=method)
    req.add_header("User-Agent", UA)
    req.add_header("Accept", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode("utf-8", "replace")
            try:
                return r.status, json.loads(raw)
            except json.JSONDecodeError:
                return r.status, raw[:600]
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace")
        try:
            return e.code, json.loads(raw)
        except json.JSONDecodeError:
            return e.code, raw[:600]


# ---------------------------------------------------------------- auth
def load_auth(path):
    with open(path) as f:
        return json.load(f)


def save_auth(path, doc):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(doc, f, indent=2)
    os.replace(tmp, path)


def refresh_token(doc, path):
    st, tr = http("POST", "/oauth/token", {
        "Content-Type": "application/x-www-form-urlencoded",
        "anthropic-beta": "oauth-2025-04-20",
    }, urllib.parse.urlencode({
        "grant_type": "refresh_token",
        "refresh_token": doc["auth"]["refreshToken"],
        "client_id": "phanthy-code-cli",
    }))
    if st != 200 or "access_token" not in (tr or {}):
        sys.exit(f"refresh 失败 HTTP {st}: {tr}")
    doc["auth"]["accessToken"] = tr["access_token"]
    doc["auth"]["refreshToken"] = tr.get("refresh_token", doc["auth"]["refreshToken"])
    doc["auth"]["expiresAt"] = int(time.time()) + int(tr.get("expires_in", 1800))
    save_auth(path, doc)
    print(f"[auth] refreshed, expires in {tr.get('expires_in', 1800)}s")
    return doc


def access_token(doc, path):
    if doc["auth"].get("expiresAt", 0) - 60 <= int(time.time()):
        doc = refresh_token(doc, path)
    return doc["auth"]["accessToken"]


# ---------------------------------------------------------------- identity
def load_or_create_identity(uid):
    path = os.path.join(AUTH_DIR, f"installation-{uid}.json")
    if os.path.exists(path):
        with open(path) as f:
            ident = json.load(f)
        ident["identitySeed"] = unb64url(ident["identitySeed"])
        ident["deviceSeed"] = unb64url(ident["deviceSeed"])
        if "sessionSeed" in ident:
            ident["sessionSeed"] = unb64url(ident["sessionSeed"])
        print(f"[identity] loaded {path}")
        return path, ident
    ident = {"identitySeed": secrets.token_bytes(32), "deviceSeed": secrets.token_bytes(32)}
    ident["installationId"] = "di_" + b64url(hashlib.sha256(public_key(ident["identitySeed"])).digest())
    ident["sessionSeed"] = secrets.token_bytes(32)
    ident["accountScope"] = uid
    save_identity(path, ident)
    print(f"[identity] created {path} -> {ident['installationId']}")
    return path, ident


def save_identity(path, ident):
    doc = dict(ident)
    for k in ("identitySeed", "deviceSeed", "sessionSeed"):
        if isinstance(doc.get(k), (bytes, bytearray)):
            doc[k] = b64url(bytes(doc[k]))
    doc["identityPublicKey"] = spki(public_key(ident["identitySeed"]))
    doc["devicePublicKey"] = spki(public_key(ident["deviceSeed"]))
    doc["sessionPublicKey"] = spki(public_key(ident["sessionSeed"]))
    with open(path, "w") as f:
        json.dump(doc, f, indent=2)
    os.chmod(path, 0o600)


# ---------------------------------------------------------------- signed call
def signed_call(token, ident, method, upstream_path, body, idem="", ts_nonce=None):
    """body: str or None. 返回 (status, json)。分隔符 "\n" 是硬约束。"""
    raw_body = "" if body is None else body
    upstream = "/api/oauth" + upstream_path
    ts, nonce = ts_nonce or (str(int(time.time() * 1000)), b64url(secrets.token_bytes(18)))
    msg = "\n".join(["v1", method, upstream, ts, nonce,
                     b64url(hashlib.sha256(raw_body.encode()).digest()), idem])
    h = {
        "Content-Type": "application/json",
        "x-desktop-installation-id": ident["installationId"],
        "x-desktop-timestamp": ts,
        "x-desktop-nonce": nonce,
        "x-desktop-signature": b64url(sign(ident["identitySeed"], msg.encode())),
        "Authorization": "Bearer " + token,
    }
    if idem:
        h["idempotency-key"] = idem
    st, raw = http(method, "/api/oauth" + upstream_path, h, raw_body if method == "POST" else None)
    return st, raw


def register(token, ident, platform=None):
    platform = platform or ("macos" if sys.platform == "darwin" else "windows")
    sess = spki(public_key(ident["sessionSeed"]))
    ts, nonce = str(int(time.time() * 1000)), b64url(secrets.token_bytes(18))
    bind = "\n".join(["desktop-device-v1", ident["accountScope"], ident["installationId"],
                      spki(public_key(ident["identitySeed"])), sess, ts, nonce])
    body = json.dumps({
        "public_key": spki(public_key(ident["identitySeed"])),
        "edition": EDITION,
        "platform": platform,
        "version": VERSION,
        "device_public_key": spki(public_key(ident["deviceSeed"])),
        "device_signature": b64url(sign(ident["deviceSeed"], bind.encode())),  # 设备钥匙签
        "inference_public_key": sess,
    }, separators=(",", ":"))
    return signed_call(token, ident, "POST", "/desktop-installations/register", body,
                       f"register:{ident['installationId']}", ts_nonce=(ts, nonce))


def summary(token, ident):
    body = json.dumps({"inference_public_key": spki(public_key(ident["sessionSeed"]))},
                      separators=(",", ":"))
    return signed_call(token, ident, "POST", "/activities/summary", body)


def claim(token, ident, round_key):
    return signed_call(token, ident, "POST", "/activities/daily-login/claim", "{}",
                       f"daily:{ident['installationId']}:{round_key}")


def rewards(token, ident):
    return signed_call(token, ident, "GET", "/rewards?type=daily_login", None)


def reconcile(token, ident):
    """让服务端按真实用量结算 onboarding 任务（不自造证据，只触发核对）。"""
    return signed_call(token, ident, "POST", "/activities/onboarding/reconcile", "{}",
                       f"onboarding:reconcile:{ident['installationId']}:latest:{int(time.time())}")


# ---------------------------------------------------------------- main
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--auth", help="auths/phanthy-*.json（默认取最新的）")
    ap.add_argument("--dry-run", action="store_true", help="只做 register/summary，不 claim")
    ap.add_argument("--status", action="store_true", help="只读：打印 summary/rewards 后退出")
    ap.add_argument("--reconcile", action="store_true", help="额外触发 onboarding 用量核对")
    ap.add_argument("--json", action="store_true", help="输出机器可读 JSON")
    args = ap.parse_args()

    auth_path = args.auth or sorted(
        (os.path.join(AUTH_DIR, f) for f in os.listdir(AUTH_DIR)
         if f.startswith("phanthy-") and f.endswith(".json")),
        key=os.path.getmtime)[-1]
    doc = load_auth(auth_path)
    token = access_token(doc, auth_path)

    st, profile = http("GET", "/api/oauth/profile", {"Authorization": "Bearer " + token})
    if st != 200:
        sys.exit(f"profile 失败 HTTP {st}: {profile}")
    uid = profile["account"]["uuid"]
    print(f"[profile] {uid}")

    ident_path, ident = load_or_create_identity(uid)

    st, reg = register(token, ident)
    print(f"[register] {st} {json.dumps(reg, ensure_ascii=False)[:260]}")
    if st not in (200, 201):
        # 当日重复 register 会 503 temporarily_unavailable（身份已在），不算失败，降级继续
        code = ((reg or {}).get("error") or {}).get("code") if isinstance(reg, dict) else None
        if code == "temporarily_unavailable":
            print(f"[register] 503 temporarily_unavailable（身份已存在，继续）")
        else:
            sys.exit(f"register 失败：{reg}")

    st, summ = summary(token, ident)
    print(f"[summary] {st} {json.dumps(summ, ensure_ascii=False)[:400]}")
    if st != 200:
        sys.exit(f"summary 失败：{summ}")

    daily = summ.get("daily") or {}
    flags = summ.get("feature_flags") or {}
    result = {"uid": uid, "summary": summ, "claim": None, "rewards": None}

    if args.reconcile:
        st, rec = reconcile(token, ident)
        print(f"[reconcile] {st} {json.dumps(rec, ensure_ascii=False)[:300]}")
        result["reconcile"] = rec

    if args.status:
        print(f"[status] daily.status={daily.get('status')} streak={daily.get('streak_day')} "
              f"next_eligible={daily.get('next_eligible_at')} "
              f"credits={(summ.get('credits') or {}).get('available')}")
        if args.json:
            print(json.dumps(result, ensure_ascii=False))
        return 0

    if args.dry_run:
        print("[claim] skipped (--dry-run)")
    elif not flags.get("daily_enabled", True):
        print("[claim] skipped: daily_enabled=false")
    elif daily.get("status") == "eligible_today":
        round_key = daily.get("server_date")
        if not round_key:
            sys.exit("summary 未给出 daily.server_date，无法 claim")
        st, cl = claim(token, ident, round_key)
        print(f"[claim] {st} {json.dumps(cl, ensure_ascii=False)[:400]}")
        result["claim"] = cl
    else:
        print(f"[claim] skipped: daily.status={daily.get('status')}（今天已领或不在窗口）")

    st, rw = rewards(token, ident)
    print(f"[rewards] {st} {json.dumps(rw, ensure_ascii=False)[:400]}")
    result["rewards"] = rw

    if args.json:
        print(json.dumps(result, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
