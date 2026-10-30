#!/usr/bin/env python3
"""桌面推理通道：desktop-inference-v2 证明 + 真实 /v1/messages 调用。

协议来源（PhanthyWork 2.43.0 macos sidecar `sw8()`）：
  canonical = "\\n".join([
    "desktop-inference-v2", METHOD, pathname, ts_ms, nonce_b64url(24B),
    b64url(sha256(body)), b64url(sha256(authorization)), b64url(sha256(desktop_inference_token)),
    x-request-id, x-claude-code-session-id, x-phanthy-expert-id, x-phanthy-skill-names,
  ])  # 后四项缺省为空串
  签名 = Ed25519(session 私钥)，即 summary 返回 token 里 inference_public_key 对应的那把
  头 = x-phanthy-desktop-session / -proof-timestamp / -proof-nonce / -proof-signature
"""
import base64
import hashlib
import subprocess
import json
import os
import platform
import secrets
import sys
import time
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import claim_daily as C
from ed25519_min import sign

PROOF_TAG = "desktop-inference-v2"


def proof_headers(ident, desktop_token, method, pathname, body_bytes, auth_header,
                  request_id="", session_id="", expert_id="", skill_names=""):
    ts = str(int(time.time() * 1000))
    nonce = C.b64url(secrets.token_bytes(24))
    h = lambda s: C.b64url(hashlib.sha256(s).digest())
    msg = "\n".join([
        PROOF_TAG, method.upper(), pathname, ts, nonce,
        h(body_bytes), h(auth_header.encode()), h(desktop_token.encode()),
        request_id, session_id, expert_id, skill_names,
    ])
    return {
        "x-phanthy-desktop-session": desktop_token,
        "x-phanthy-proof-timestamp": ts,
        "x-phanthy-proof-nonce": nonce,
        "x-phanthy-proof-signature": C.b64url(sign(ident["sessionSeed"], msg.encode())),
    }


def new_session_id():
    return str(uuid.uuid4())


def jwt_claims(token):
    p = token.split(".")[1]
    p += "=" * (-len(p) % 4)
    return json.loads(base64.urlsafe_b64decode(p))


CLI_VERSION = "2.1.92"       # 内置内核版本，UA 与 cli-device 用它
CLIENT_VERSION = "2.43.0"    # 桌面端版本，x-phanthy-client-version 用它


def _lan_ips():
    """本机非回环 IPv4；真桌面端会把 os.networkInterfaces() 的地址报上去。"""
    try:
        out = subprocess.run(["ifconfig"], capture_output=True, text=True).stdout
    except Exception:
        return []
    ips = []
    for line in out.splitlines():
        line = line.strip()
        if line.startswith("inet ") and not line.startswith("inet 127."):
            ips.append(line.split()[1])
    return sorted(set(ips))[:2]


def device_profile(ident):
    """由身份种子派生稳定画像（与 Go 版 internal/fingerprint 同构）。"""
    ss = ident["sessionSeed"]
    if isinstance(ss, bytes):
        seed = ss
    else:
        seed = base64.urlsafe_b64decode(ss + "=" * (-len(ss) % 4))
    h = lambda tag: hashlib.sha256(seed + b"\0" + tag.encode()).digest()
    assert sys.platform == "darwin" or True
    prof = {
        "platform": "darwin",
        "arch": "x64",
        "osRelease": "24.6.0",
        "deviceId": h("device")[:16].hex(),
        "ips": _lan_ips(),
    }
    if h("platform")[0] % 10 >= 8:
        prof["platform"], prof["osRelease"] = "win32", "10.0.22631"
    return prof


def cli_device(prof=None, version=CLI_VERSION):
    """x-phanthy-cli-device：服务端用它填 usage 的 client_os；缺了 client_os 为 null。"""
    prof = prof or {"platform": "darwin" if sys.platform == "darwin" else "win32",
                    "arch": "x64", "osRelease": platform.release(), "ips": _lan_ips()}
    doc = {"version": 1, "platform": prof["platform"], "arch": prof["arch"],
           "osRelease": prof["osRelease"], "cliVersion": version,
           "ipCandidates": prof.get("ips") or []}
    return C.b64url(json.dumps(doc, separators=(",", ":")).encode())


def flatten_system(body):
    """上游只认 system 字符串：数组会被整段忽略（实测）。"""
    sysv = body.get("system")
    if isinstance(sysv, list):
        parts = [p.get("text", "") for p in sysv if isinstance(p, dict)]
        body["system"] = "\n\n".join(t for t in parts if t)
    return body


def pick_auth():
    """没指定 --auth 时取 auths/ 下最新的 phanthy-*.json（不硬编码账号文件名）。"""
    import glob
    cands = sorted(glob.glob(os.path.join(C.AUTH_DIR, "phanthy-*.json")),
                   key=os.path.getmtime, reverse=True)
    if not cands:
        raise SystemExit(f"{C.AUTH_DIR} 下没有 phanthy-*.json，请先登录或指定 --auth")
    return cands[0]


class Desktop:
    def __init__(self, auth_path=None, scope=None):
        self.auth_path = auth_path or pick_auth()
        self.doc = C.load_auth(self.auth_path)
        self.token = C.access_token(self.doc, self.auth_path)
        scope = scope or self.doc.get("accountScope") or jwt_claims(self.token).get("sub")
        _, self.ident = C.load_or_create_identity(scope)
        C.register(self.token, self.ident)
        st, summ = C.summary(self.token, self.ident)
        if st != 200:
            raise SystemExit(f"summary {st}: {summ}")
        self.summary = summ
        self.desktop_token = summ["desktop_inference_token"]

    def chat(self, body, session_id="", expert_id="", skill_names="", request_id=""):
        path = "/v1/messages"
        prof = device_profile(self.ident)
        flatten_system(body)
        request_id = request_id or str(uuid.uuid4())
        sid = session_id or str(uuid.uuid4())
        # metadata.user_id：桌面端发的是 JSON 串，device_id 固定为这台机器
        md = body.setdefault("metadata", {})
        if os.getenv("NO_MD") == "1":
            md.pop("user_id", None)
        else:
            md.setdefault("user_id", json.dumps(
                {"device_id": prof["deviceId"], "account_uuid": self.ident.get("accountScope", ""), "session_id": sid},
                separators=(",", ":")))
        raw = json.dumps(body, separators=(",", ":")).encode()
        auth = "Bearer " + self.token
        hdr = {
            "Content-Type": "application/json",
            "Accept": "application/json",
            "anthropic-version": "2023-06-01",
            "anthropic-beta": "claude-code-20250219,oauth-2025-04-20",
            "Authorization": auth,
            "User-Agent": f"claude-cli/{CLI_VERSION} (external, cli)",
            "x-app": "cli",
            "x-phanthy-client-source": "desktop",
            "x-phanthy-client-version": CLIENT_VERSION,
            "x-phanthy-cli-device": cli_device(prof),
            # 两个 id 头都必须与签名串一致；只发 x-client-request-id 会 403
            "x-client-request-id": request_id,
            "x-request-id": request_id,
            "X-Claude-Code-Session-Id": sid,
        }
        # 两个会话头都要：服务端按 X-PHANTHY-SESSION 判定会话来自桌面端，
        # 按 X-CLAUDE-CODE-SESSION-ID 认会话；缺一个都会让会话/缓存判定失效。
        # 只发一个小写名：urllib 的 header 大小写敏感，同时发 X-Claude-Code-Session-Id 和
        # x-claude-code-session-id 会变成两个同名头，服务端读到的值不是单个 sid，
        # 导致 reconcile evidence 里 signed_session_match=false（onboarding 永远结算不了）。
        hdr["x-phanthy-session"] = sid
        if expert_id:
            hdr["x-phanthy-expert-id"] = expert_id
        if skill_names:
            hdr["x-phanthy-skill-names"] = skill_names
        hdr.update(proof_headers(self.ident, self.desktop_token, "POST", path, raw, auth,
                                 request_id=request_id, session_id=sid,
                                 expert_id=expert_id, skill_names=skill_names))
        return C.http("POST", path + "?beta=true", hdr, raw.decode(), timeout=120)


def main():
    ap = __import__("argparse").ArgumentParser()
    ap.add_argument("--model", default="Iris-1.0")
    ap.add_argument("--prompt", default="你好，用一句话介绍你自己")
    ap.add_argument("--max-tokens", type=int, default=64)
    ap.add_argument("--session", default="")
    ap.add_argument("--expert", default="")
    ap.add_argument("--skills", default="")
    ap.add_argument("--raw", action="store_true")
    a = ap.parse_args()
    d = Desktop()
    print("[token]", d.desktop_token[:24] + "...", "exp_in", 180)
    st, raw = d.chat({"model": a.model, "max_tokens": a.max_tokens, "stream": False,
                      "messages": [{"role": "user", "content": a.prompt}]},
                     session_id=a.session, expert_id=a.expert, skill_names=a.skills)
    print("[messages]", st)
    print(raw if a.raw or not isinstance(raw, dict) else json.dumps(raw, ensure_ascii=False)[:1200])


if __name__ == "__main__":
    main()
