#!/usr/bin/env python3
"""pprotein のグループの生データ(httplog/slowlog/pprof)と解析結果を手元に保存する。

usage: python3 scripts/pprotein_download.py [GROUP_ID|latest] [--base http://18.179.119.86] [--out pprotein-data]
保存先: <out>/<GROUP_ID>/ に生データ、<ID>.result(解析結果)、manifest.json
あとから scripts/pprotein_replay.py で pprotein に読み直させられる。
"""
import argparse, json, os, urllib.request

def get(url, timeout=300):
    with urllib.request.urlopen(url, timeout=timeout) as r:
        return r.read()

p = argparse.ArgumentParser()
p.add_argument("group", nargs="?", default="latest")
p.add_argument("--base", default="http://18.179.119.86")
p.add_argument("--out", default=os.path.join(os.path.dirname(__file__), "..", "pprotein-data"))
a = p.parse_args()
api = a.base.rstrip("/") + "/api"

entries = []
for t in ("httplog", "slowlog", "pprof"):
    for e in json.loads(get(f"{api}/{t}")):
        if e.get("Status") == "ok":
            entries.append(e["Snapshot"])
group = a.group if a.group != "latest" else max(s["GroupId"] for s in entries)
snaps = [s for s in entries if s["GroupId"] == group]
if not snaps:
    raise SystemExit(f"group not found: {group}")

d = os.path.join(a.out, group)
os.makedirs(d, exist_ok=True)
manifest = []
for s in snaps:
    t, sid = s["Type"], s["ID"]
    with open(os.path.join(d, sid), "wb") as f:
        f.write(get(f"{api}/{t}/data/{sid}"))
    if t != "pprof":
        with open(os.path.join(d, sid + ".result.tsv"), "wb") as f:
            f.write(get(f"{api}/{t}/{sid}"))
    manifest.append({"type": t, "id": sid, "label": s["Label"], "duration": s["Duration"],
                     "url": s["URL"], "commit": s["Repository"].get("Hash", "")})
    print(f"saved {t} {s['Label']} {sid}")
with open(os.path.join(d, "manifest.json"), "w") as f:
    json.dump({"group": group, "snapshots": manifest}, f, indent=1, ensure_ascii=False)
print(d)
