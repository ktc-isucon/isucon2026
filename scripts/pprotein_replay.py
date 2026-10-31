#!/usr/bin/env python3
"""pprotein_download.py で保存したグループを、pprotein にもう一度取り込ませる。

pprotein は登録された URL からデータを取りに来る仕組みなので、保存したファイルをこのスクリプトが
HTTP で配り、その URL を pprotein に登録する(POST /api/<type>)。取り込みが終わったら配信を止める。
pprotein から --serve-host:--port に届く場所で実行すること(例: pprotein のホスト上で
--serve-host 127.0.0.1、またはそのポートを開けたサーバー上でそのサーバーのIP)。

usage:
  python3 scripts/pprotein_replay.py pprotein-data/<GROUP_ID> --serve-host <pproteinから見えるIP> [--port 19100]
      [--base http://18.179.119.86] [--label-suffix -replay] [--dry-run]
"""
import argparse, functools, http.server, json, os, threading, time, urllib.request

p = argparse.ArgumentParser()
p.add_argument("dir")
p.add_argument("--serve-host", required=True)
p.add_argument("--port", type=int, default=19100)
p.add_argument("--base", default="http://18.179.119.86")
p.add_argument("--label-suffix", default="-replay")
p.add_argument("--timeout", type=int, default=300)
p.add_argument("--dry-run", action="store_true")
a = p.parse_args()
api = a.base.rstrip("/") + "/api"
m = json.load(open(os.path.join(a.dir, "manifest.json")))

class Handler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print("served", self.path)

srv = http.server.ThreadingHTTPServer(("0.0.0.0", a.port),
                                      functools.partial(Handler, directory=os.path.abspath(a.dir)))
threading.Thread(target=srv.serve_forever, daemon=True).start()

posted = []
for s in m["snapshots"]:
    body = {"Label": s["label"] + a.label_suffix,
            "URL": f"http://{a.serve_host}:{a.port}/{s['id']}",
            "Duration": 1}
    print("POST", s["type"], json.dumps(body))
    if a.dry_run:
        # 自分自身から取得できるかだけ確かめる
        with urllib.request.urlopen(f"http://127.0.0.1:{a.port}/{s['id']}", timeout=30) as r:
            print("  local fetch ok:", len(r.read()), "bytes")
        continue
    req = urllib.request.Request(f"{api}/{s['type']}", data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=30) as r:
        r.read()
    posted.append((s["type"], body["Label"]))

# pprotein が取りに来て処理を終えるまで待つ
deadline = time.time() + a.timeout
pending = set(posted)
while pending and time.time() < deadline:
    time.sleep(2)
    for t in {t for t, _ in pending}:
        for e in json.loads(urllib.request.urlopen(f"{api}/{t}", timeout=30).read()):
            key = (t, e["Snapshot"]["Label"])
            if key in pending and e["Status"] != "pending":
                print("done", t, key[1], e["Status"], e.get("Message", ""), "group", e["Snapshot"]["GroupId"])
                pending.discard(key)
if pending:
    print("timeout:", pending)
srv.shutdown()
