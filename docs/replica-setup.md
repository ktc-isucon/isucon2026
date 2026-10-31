# 読み取り用レプリカの導入手順(まず isu0)

isu3 を primary のまま、アプリのサーバーに非同期のストリーミングレプリカを置く。
アプリは読み取り専用のリクエストだけ、primary の flush 位置までレプリカが反映したのを確かめてから手元で読む(`webapp/go/replica.go`)。
書き込みと行ロックはこれまでどおり primary で行うので、書き込みは遅くならない。

**まず isu0(asia)だけをレプリカにする。** 今の律速は isu3 の CPU で、その大半は asia の読み取り(ユーザー検索・フィード)。
isu0⇔isu3 は往復0.5msなので、反映位置の確認もほぼタダで、初期同期(約17GB)も速い。
isu1・isu2 は、isu0 で効果と安定性を確かめてから、あとで同じ手順で広げる(下の「isu1・isu2 に広げる」)。

## 仕組みと正しさ

1. 読み取り専用のリクエストの最初に、primary へ `SELECT pg_current_wal_flush_lsn()` を送る(1往復)。
2. 手元のレプリカで `pg_last_wal_replay_lsn() >= その位置` になるまで、1msおきに確かめる(最大200ms)。
3. 追いついたら、そのリクエストの読み取り(セッション確認を含む)をすべてレプリカで行う。
4. 追いつかない、またはレプリカでエラーになったときは primary で読む。

`synchronous_commit = on` なので、成功を返した書き込みのコミット記録は、必ず flush 位置より前にある。
よって、手順1より前に成功した書き込みは、どのリージョンのものでも手順3で必ず見える(マニュアルの「書き込み後の読み取りに反映」を満たす)。

`ISUREAL_REPLICA_DB_HOST` を設定しなければ、アプリは今までどおり primary だけで動く。外したいときも、この行を消してアプリを再起動すればよい。

対象のAPIは `server.go` で `ReplicaReadMiddleware` を付けた読み取り専用のルート(フィード、メモリーズ、ユーザー検索・詳細、投稿の詳細・コメント/RealMoji の一覧、今の投稿期間、自分の投稿、エクスポート、画像など)。
SSE(`/api/notifications`)と、すべての書き込みは primary のまま。

## リポジトリ側で用意したもの

| ファイル | 内容 |
|---|---|
| `etc/isu3/.../pg_hba.conf` | `replicator` の replication 接続を 192.168.1.10・2.10・3.10 から許可 |
| `etc/isu3/.../postgresql.conf` | `max_slot_wal_keep_size = 2GB`(レプリカが止まっても WAL でディスクがあふれない) |
| `etc/isu0/.../postgresql.conf` | レプリカ用。`max_connections = 200` など primary 以上が必要な値、`hot_standby_feedback = on`、`max_standby_streaming_delay = 5s`、`shared_buffers = 512MB`(アプリと同居)。重いログ(auto_explain・jsonlog)はなし |
| `scripts/replica_primary_setup.sh` | isu3 で実行。`replicator` ロールと slot を作る(何度実行してもよい) |
| `scripts/replica_sync.sh` | レプリカで実行。手元の古いデータを消し、`pg_basebackup -R -S <slot> -X stream` で取り直して起動 |
| `scripts/replica_teardown.sh` | slot の削除(isu3)とレプリカの停止 |

パスワードはリポジトリに置かない。`REPLICA_PASSWORD` 環境変数で渡し、`pg_basebackup -R` がレプリカのデータディレクトリの `postgresql.auto.conf` に書く。

## 手順(isu0)

ベンチを流していないときに行う。`<パスワード>` は好きな文字列に置き換える(isu3 とレプリカで同じもの)。

```sh
# 0. isu3 のディスクを空ける(slot が WAL を持つので先に)
ssh isu3 'sudo truncate -s 0 /var/log/postgresql/postgresql-slow.json /var/log/postgresql/postgresql-slow.log /var/log/postgresql/postgresql-slow-mysql.log; df -h / | tail -1'

# 1. 設定を配る(isu3: pg_hba と max_slot_wal_keep_size。PostgreSQL が再起動する / isu0: レプリカ用の postgresql.conf。isu0 の PostgreSQL はまだ止まったまま)
#    main を pull したうえで
task push:all -- isu3
task push:all -- isu0
ssh isu0 'git -C /home/isucon log --oneline -1'   # isu0 にスクリプトが届いていること

# 2. isu3 で replicator ロールと slot(isu0)を作る
ssh isu3 "REPLICA_PASSWORD='<パスワード>' bash /home/isucon/scripts/replica_primary_setup.sh isu0"

# 3. isu0 で初期同期して、レプリカとして起動する(約17GB、数分)
ssh isu0 "REPLICA_PASSWORD='<パスワード>' bash /home/isucon/scripts/replica_sync.sh isu0"

# 4. 確認
ssh isu0 'sudo -u postgres psql -Atc "SELECT pg_is_in_recovery(), pg_last_wal_replay_lsn()"'                      # t と LSN
ssh isu3 'sudo -u postgres psql -Atc "SELECT application_name, state, replay_lag FROM pg_stat_replication"'      # isu0|streaming

# 5. アプリをレプリカに向ける
ssh isu0 'echo ISUREAL_REPLICA_DB_HOST=127.0.0.1 >> /home/isucon/env.sh && sudo systemctl restart isureal-go'
```

## 確認(ベンチ)

- 整合性チェックが通ること。
- isu3 の load average と、slowlog の上位(ユーザー検索・フィード)が下がること。
- isu0 の CPU が張り付かないこと(アプリとレプリカが2コアを取り合う)。張り付くなら手順5の行を消して戻す。
- isu0 のアプリのログに、レプリカでのエラー(500)が出ないこと。
- isu3 の `pg_stat_replication` の `replay_lag` が小さい(数十ms以下)まま保たれること。

## 初期化(reset.sql)

primary で実行すれば、その WAL がレプリカにも流れる。`TRUNCATE` は強いロックを取るので、レプリカで読み取り中のクエリがあると、反映が最大 `max_standby_streaming_delay`(5秒)待たされる。その間に来た読み取りは200msの待ちのあと primary で読むので、正しさは変わらない。

## 止め方

```sh
ssh isu0 "sed -i '/^ISUREAL_REPLICA_DB_HOST=/d' /home/isucon/env.sh && sudo systemctl restart isureal-go"   # アプリを先に外す
ssh isu3 'bash /home/isucon/scripts/replica_teardown.sh primary isu0'   # slot を消す(消さないと WAL がたまる)
ssh isu0 'bash /home/isucon/scripts/replica_teardown.sh replica'        # isu0 の PostgreSQL を止める
```

## isu1・isu2 に広げる(あとで)

isu0 と同じ流れで、`etc/isu1`・`etc/isu2` の `postgresql.conf` にも isu0 と同じレプリカ用の設定を入れ、slot `isu1`・`isu2` を作る。
ただし `pg_basebackup` は約17GBを丸ごと送るので、帯域制限(1接続1〜2MB/s)のある isu1・isu2 では数時間かかる。
isu0〜3 は同じクラスタのコピーなので、`pg_backup_start` → rsync(`--checksum`、primary 側は `--rsync-path="sudo rsync"`)で差分だけ送り、
`pg_backup_stop` の `labelfile` を `backup_label` に置く方式なら短くできる見込み(サーバー間の SSH 鍵が必要)。必要になったら手順を足す。

## リスク

- **WAL とディスク**: slot はレプリカが受け取るまで WAL を isu3 に残す。`max_slot_wal_keep_size = 2GB` を超えると slot が無効になり、レプリカは `replica_sync.sh` で取り直しになる(isu3 のディスクはあふれない)。レプリカを止めたら slot を消す。
- **CPU**: isu0 は2コアでアプリとレプリカが同居する。
- **max_connections の整合**: レプリカの `max_connections`・`max_worker_processes`・`max_wal_senders`・`max_prepared_transactions`・`max_locks_per_transaction` は primary 以上でないと hot standby が起動しない。primary 側を上げたら、レプリカ側も先に上げる。
- **設定の上書き**: isuconf は `etc/isuN/` の `postgresql.conf` を各サーバーに配る。isu0 の `etc/isu0/.../postgresql.conf` はレプリカ用なので、primary 用の値で上書きしないこと。`taskfile.yaml` の `restart:postgresql` は isu3 だけを再起動するので、isu0 のレプリカの設定を変えたときは `ssh isu0 'sudo systemctl restart postgresql'` を手で行う。
- **クエリのキャンセル**: レプリカで長いクエリが反映と衝突すると、`max_standby_streaming_delay`(5秒)を超えた時点でキャンセルされ500になる可能性がある(`hot_standby_feedback = on` でほとんど防げる)。
- **反映の遅れ**: レプリカの反映が200ms以上遅れると、そのリクエストは primary に戻り、待った分だけ遅くなる。
