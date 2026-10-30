#!/usr/bin/env python3
"""验证「哪些模型真的能用」：对每个模型 id 发一次最小真实请求，记录上游是否放行。

只花极少积分（每次 max_tokens=16 的 ping），不构造 usage、不改任何状态。

用法：
  python3 scripts/probe_models.py                    # 内置 13 个候选（网页上列出的）
  python3 scripts/probe_models.py --models a,b,c     # 指定
  python3 scripts/probe_models.py --from-config      # 读 config.json 的 models 键
  python3 scripts/probe_models.py --auth auths/phanthy-<ts>.json --json data/models_probe.json
"""
import argparse
import json
import os
import sys
import time
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import inference as I

# 网页（code.phanthy.com）上列出的模型；上游目录比客户端内置表新，所以要实测
CANDIDATES = [
    "Phanthy-fast", "Phanthy-pro", "Phanthy-ultra",
    "glm-5.3-flash", "glm-5.3", "glm-5.2",
    "kimi-k3", "kimi-k2.7-code",
    "deepseek-v4.1-flash-1017",
    "Phana-5.5o", "Phana-5.5s", "Phano-6a", "Phano-6.1s",
]


def probe(doc, model):
    sid = str(uuid.uuid4())
    t0 = time.time()
    st, raw = doc.chat({"model": model, "max_tokens": 16, "stream": False,
                        "messages": [{"role": "user", "content": "只回复一个字：好"}]},
                       session_id=sid)
    dt = round(time.time() - t0, 1)
    if not isinstance(raw, dict):
        return {"model": model, "http": st, "ok": False, "error": str(raw)[:160], "secs": dt}
    err = (raw.get("error") or {}) if isinstance(raw.get("error"), dict) else {}
    u = raw.get("usage") or {}
    return {
        "model": model,
        "http": st,
        "ok": st == 200,
        "served_model": raw.get("model"),
        "error": err.get("code") or err.get("message") or None,
        "in": u.get("input_tokens"), "out": u.get("output_tokens"),
        "stop": raw.get("stop_reason"),
        "secs": dt,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--models", default="", help="逗号分隔的模型 id")
    ap.add_argument("--from-config", action="store_true", help="读 config.json 的 models 键")
    ap.add_argument("--auth", default="")
    ap.add_argument("--json", default="", help="结果落盘路径")
    ap.add_argument("--delay", type=float, default=2.0)
    a = ap.parse_args()

    if a.models:
        models = [m.strip() for m in a.models.split(",") if m.strip()]
    elif a.from_config:
        cfg = json.load(open("config.json", encoding="utf-8"))
        models = list((cfg.get("models") or {}).keys())
    else:
        models = CANDIDATES

    doc = I.Desktop(auth_path=a.auth or None)
    print(f"[probe] {len(models)} 个模型，账号 credits={(doc.summary.get('credits') or {}).get('available')}")
    rows = []
    for i, m in enumerate(models, 1):
        r = probe(doc, m)
        rows.append(r)
        flag = "OK  " if r["ok"] else "FAIL"
        served = r.get("served_model") or "-"
        print(f"  {i:2d}/{len(models)} {flag} {m:28s} http={r['http']:<3} "
              f"served={served:<22} out={r.get('out')} {r['secs']}s"
              + ("" if r["ok"] else f"  err={r.get('error')}"))
        time.sleep(a.delay)

    ok = [r for r in rows if r["ok"]]
    bad = [r for r in rows if not r["ok"]]
    print(f"\n[结果] 放行 {len(ok)}/{len(rows)}；被拒 {len(bad)}")
    for r in bad:
        print(f"   ✗ {r['model']}: http {r['http']} {r.get('error')}")
    # 上游把别名档位路由到别的模型时，served 与请求名不一致，单独点出来
    alias = [r for r in ok if (r.get("served_model") or "").lower() != r["model"].lower()]
    for r in alias:
        print(f"   ~ {r['model']} 实际由 {r['served_model']} 服务")

    if a.json:
        os.makedirs(os.path.dirname(os.path.abspath(a.json)), exist_ok=True)
        json.dump({"probed_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"), "rows": rows},
                  open(a.json, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
        print(f"[写入] {a.json}")


if __name__ == "__main__":
    main()
