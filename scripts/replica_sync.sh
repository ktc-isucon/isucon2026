#!/bin/bash
# レプリカにするサーバー(まずは isu0)上で実行する。手元の古いデータを捨て、isu3(primary)から
# pg_basebackup で丸ごと取り直して、ストリーミングレプリカとして起動する。
# 先に isu3 で scripts/replica_primary_setup.sh を実行し、ロールと slot を作っておくこと。
#
# usage: REPLICA_PASSWORD='<パスワード>' bash /home/isucon/scripts/replica_sync.sh isu0
#
# - 約17GBを転送する。isu0⇔isu3 は同じリージョンなので数分の見込み。
#   isu1・isu2 は帯域制限があり数時間かかるので、そのまま流さないこと(docs/replica-setup.md 参照)。
# - 手元の /var/lib/postgresql/16/main は消える(配布時点の古いコピーなので、アプリは使っていない)。
# - sudo が必要。primary への接続はプライベートIP(ルーター経由)のみ。
set -euo pipefail

name="${1:-}"
case "$name" in
  isu0|isu1|isu2) ;;
  *) echo "usage: REPLICA_PASSWORD=... $0 isu0" >&2; exit 1 ;;
esac
if [ -z "${REPLICA_PASSWORD:-}" ]; then
  echo "REPLICA_PASSWORD を設定してください" >&2
  exit 1
fi

PRIMARY_HOST=192.168.4.10
DATA_DIR=/var/lib/postgresql/16/main
PG_BIN=/usr/lib/postgresql/16/bin

# レプリカ用の設定(max_connections などが primary 以上)が入っているか確かめる。
# 入っていないと hot standby が起動しない
if ! grep -q '^hot_standby_feedback = on' /etc/postgresql/16/main/postgresql.conf; then
  echo "/etc/postgresql/16/main/postgresql.conf がレプリカ用ではありません。先に task push:all -- $name で反映してください" >&2
  exit 1
fi

echo "== 手元の PostgreSQL を止めて、古いデータを消す"
sudo systemctl stop postgresql
sudo find "$DATA_DIR" -mindepth 1 -delete

echo "== isu3 から pg_basebackup(slot=$name)"
# -R で standby.signal と primary_conninfo / primary_slot_name を postgresql.auto.conf に書く
sudo -u postgres env PGPASSWORD="$REPLICA_PASSWORD" "$PG_BIN/pg_basebackup" \
  -d "host=$PRIMARY_HOST port=5432 user=replicator application_name=$name" \
  -D "$DATA_DIR" -X stream -S "$name" -R -c fast -P
sudo chmod 700 "$DATA_DIR"

echo "== レプリカとして起動(再起動後も上がるよう enable)"
sudo systemctl enable postgresql
sudo systemctl start postgresql

for _ in $(seq 1 30); do
  if sudo -u postgres psql --no-psqlrc -qAtX -c 'SELECT 1' >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
sudo -u postgres psql --no-psqlrc -qAtX -c \
  "SELECT 'in_recovery=' || pg_is_in_recovery(), 'replay_lsn=' || pg_last_wal_replay_lsn(), 'receive_lsn=' || pg_last_wal_receive_lsn()"
echo "in_recovery=t で LSN が出ていれば成功。isu3 で pg_stat_replication に $name が streaming で出ることも確かめる"
