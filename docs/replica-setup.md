# 読み取り用レプリカ(isu1・isu2)の導入手順

isu3 を primary のまま、isu1(europe)と isu2(america)に非同期のストリーミングレプリカを置く。
アプリは読み取り専用のリクエストだけ、primary の flush 位置までレプリカが反映したのを確かめてから手元で読む(`webapp/go/replica.go`)。
書き込みと行ロックはこれまでどおり primary で行うので、asia の書き込みは遅くならない。

isu0 は isu3 まで往復0.5msなので対象外。

## 仕組みと正しさ

1. 読み取り専用のリクエストの最初に、primary へ `SELECT pg_current_wal_flush_lsn()` を送る(1往復)。
2. 手元のレプリカで `pg_last_wal_replay_lsn() >= その位置` になるまで、1msおきに確かめる(最大200ms)。
3. 追いついたら、そのリクエストの読み取り(セッション確認を含む)をすべてレプリカで行う。
4. 追いつかない、またはレプリカでエラーになったときは primary で読む。

`synchronous_commit = on` なので、成功を返した書き込みのコミット記録は、必ず flush 位置より前にある。
よって、手順1より前に成功した書き込みは、どのリージョンのものでも手順3で必ず見える(マニュアルの「書き込み後の読み取りに反映」を満たす)。

`ISUREAL_REPLICA_DB_HOST` を設定しなければ、アプリは今までどおり primary だけで動く。レプリカを外したいときも、この行を消してアプリを再起動すればよい。

## 対象のAPI

`GET /api/users`、`/api/users/:userId`、`/api/me`、`/api/me/export`、`/api/posting-window/current`、`/api/posts/me/current`、`/api/posts/:postId`、`/api/feeds/:feed`、`/api/memories`、`/api/memories/:date`、`/api/posts/:postId/comments`(GET)、`/api/posts/:postId/realmojis`(GET)、`/api/relationships/:kind`、`/api/images/:imageId`。

SSE(`/api/notifications`)と、すべての書き込みは primary のまま。

## 前提

- isu0〜isu3 の PostgreSQL は同じクラスタのコピー(system identifier が同じ)。isu1・isu2 のデータは配布時点で止まっているので、差分だけを rsync で送れる。
- isu3 は `wal_level = replica`、`max_wal_senders = 10`。
- isu3 のディスクの空きは約14GB。replication slot が WAL を保持するので、レプリカが止まったままだと WAL がたまり続ける。止めたレプリカのスロットは必ず消すこと。
- 通信はすべてプライベートIP(デフォルトゲートウェイ=ルーター経由)。

## 手順

### 1. isu3(primary)

`etc/isu3/etc/postgresql/16/main/pg_hba.conf` に追加する。

```
host replication replicator 192.168.2.10/32 scram-sha-256
host replication replicator 192.168.3.10/32 scram-sha-256
```

`postgresql.conf` の追加は不要(必要なら `wal_keep_size = 1GB` を足すと、スロットなしでも短い停止に耐えられる)。

```sh
ssh isu3 "sudo -u postgres psql -c \"CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD '<パスワード>'\""
ssh isu3 "sudo -u postgres psql -c \"SELECT pg_create_physical_replication_slot('isu1')\" -c \"SELECT pg_create_physical_replication_slot('isu2')\""
cd scripts && ./isuconf push --server-names isu3 /etc/postgresql/16/main/pg_hba.conf
ssh isu3 "sudo systemctl reload postgresql"
```

### 2. isu1・isu2 へデータを同期する(各サーバーで)

primary をバックアップモードにして差分を送る。`pg_backup_start` を実行したセッションは、`pg_backup_stop` まで開いたままにする必要があるので、isu3 で psql を開いたまま作業する。

```sh
# isu3 で(このセッションを閉じない)
sudo -u postgres psql
  SELECT pg_backup_start('replica', true);
```

```sh
# isu1(isu2 も同じ)で。手元の PostgreSQL は止めておく
sudo systemctl stop postgresql
sudo rsync -a --delete --checksum \
  --exclude postmaster.pid --exclude postmaster.opts --exclude 'pg_wal/*' --exclude 'pg_replslot/*' \
  --exclude postgresql.auto.conf \
  isucon@192.168.4.10:/var/lib/postgresql/16/main/ /var/lib/postgresql/16/main/
```

rsync には isu1→isu3 の SSH 接続と、isu3 側で `/var/lib/postgresql/16/main` を読める権限(`--rsync-path="sudo rsync"`)が必要。

```sh
# isu3 の psql で
  SELECT * FROM pg_backup_stop(true);
```

`pg_backup_stop` が返す `labelfile` の内容を、isu1 の `/var/lib/postgresql/16/main/backup_label` に書く(rsync の後、起動の前)。

### 3. isu1・isu2 をレプリカとして起動する

```sh
sudo -u postgres touch /var/lib/postgresql/16/main/standby.signal
sudo -u postgres tee -a /var/lib/postgresql/16/main/postgresql.auto.conf <<'EOF'
primary_conninfo = 'host=192.168.4.10 port=5432 user=replicator password=<パスワード> application_name=isu1'
primary_slot_name = 'isu1'
hot_standby = on
hot_standby_feedback = on
max_standby_streaming_delay = 5s
EOF
sudo systemctl start postgresql
sudo -u postgres psql -Atc "SELECT pg_is_in_recovery(), pg_last_wal_replay_lsn()"   # t と LSN が出ればOK
```

isu3 で `SELECT application_name, state, replay_lsn FROM pg_stat_replication;` に isu1・isu2 が `streaming` で出ることを確かめる。

追試でサーバーを再起動しても動くよう、`sudo systemctl enable postgresql` もしておく。

### 4. アプリを向ける

isu1・isu2 の `/home/isucon/env.sh` に追加して、アプリを再起動する。

```
ISUREAL_REPLICA_DB_HOST=127.0.0.1
```

### 5. 確認

- ベンチで整合性チェックが通ること。
- isu1・isu2 の `GET /api/feeds/*` などの最小値が、約0.1秒(LSNを聞く1往復)まで下がること。
- レプリカの PostgreSQL を止めても、アプリが primary で読み続けること(待ちが失敗して primary に戻る)。

## 初期化(reset.sql)

primary で実行すれば、その WAL がレプリカにも流れる。`TRUNCATE` は強いロックを取るので、レプリカで読み取り中のクエリがあると、反映が最大 `max_standby_streaming_delay`(5秒)待たされる。その間に来た読み取りは200msの待ちのあと primary で読むので、正しさは変わらない。

## 止め方

1. isu1・isu2 の env.sh から `ISUREAL_REPLICA_DB_HOST` を消してアプリを再起動する。
2. isu3 でスロットを消す: `SELECT pg_drop_replication_slot('isu1'); SELECT pg_drop_replication_slot('isu2');`(消さないと WAL がたまる)。
3. isu1・isu2 の PostgreSQL を止める。

## リスク

- レプリカの反映が200ms以上遅れると、そのリクエストは primary に戻り、待った分だけ遅くなる。
- レプリカ側で長いクエリが反映と衝突すると、`max_standby_streaming_delay` を超えた時点でそのクエリがキャンセルされ、500 になる可能性がある(`hot_standby_feedback = on` でほとんど防げる)。
- isu1・isu2 は2コアなので、アプリとレプリカが CPU を取り合う。
