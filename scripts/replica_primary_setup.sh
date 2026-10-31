#!/bin/bash
# isu3(primary)上で実行する。読み取り用レプリカのためのロールと replication slot を用意する(何度実行してもよい)。
# パスワードはリポジトリに置かないよう、環境変数で渡す。
# usage: REPLICA_PASSWORD='<パスワード>' bash /home/isucon/scripts/replica_primary_setup.sh isu0 [isu1 isu2 ...]
set -euo pipefail

if [ -z "${REPLICA_PASSWORD:-}" ]; then
  echo "REPLICA_PASSWORD を設定してください" >&2
  exit 1
fi
if [ "$#" -eq 0 ]; then
  echo "usage: REPLICA_PASSWORD=... $0 isu0 [isu1 isu2 ...]" >&2
  exit 1
fi

psql_postgres() {
  sudo -u postgres psql --no-psqlrc -v ON_ERROR_STOP=1 -qAtX "$@"
}

# replicator ロール(なければ作り、あればパスワードを合わせる)
psql_postgres -v pw="$REPLICA_PASSWORD" <<'EOF'
SELECT format('CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD %L', :'pw')
  WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'replicator') \gexec
SELECT format('ALTER ROLE replicator WITH REPLICATION LOGIN PASSWORD %L', :'pw') \gexec
EOF
echo "role replicator: ok"

# レプリカごとの physical replication slot(なければ作る)
for name in "$@"; do
  case "$name" in
    isu0|isu1|isu2) ;;
    *) echo "unknown replica: $name" >&2; exit 1 ;;
  esac
  psql_postgres -v slot="$name" <<'EOF'
SELECT pg_create_physical_replication_slot(:'slot')
  WHERE NOT EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = :'slot');
EOF
  echo "slot $name: ok"
done

# pg_hba.conf の replicator の行を読み直す(isuconf で配置済みであること)
if ! sudo grep -q '^host[[:space:]]\+replication[[:space:]]\+replicator' /etc/postgresql/16/main/pg_hba.conf; then
  echo "pg_hba.conf に replicator の行がありません。先に task push:all -- isu3 で反映してください" >&2
  exit 1
fi
sudo systemctl reload postgresql
echo "reload: ok"

psql_postgres -c "SELECT slot_name, active, restart_lsn FROM pg_replication_slots ORDER BY 1"
