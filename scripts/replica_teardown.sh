#!/bin/bash
# 読み取り用レプリカを止める。先にアプリをレプリカから外すこと:
#   レプリカのサーバーの /home/isucon/env.sh から ISUREAL_REPLICA_DB_HOST の行を消し、アプリを再起動する。
#
# usage:
#   isu3 で:          bash /home/isucon/scripts/replica_teardown.sh primary isu0   # slot を消す(消さないと WAL がたまる)
#   レプリカのサーバーで: bash /home/isucon/scripts/replica_teardown.sh replica        # 手元の PostgreSQL を止める
set -euo pipefail

mode="${1:-}"
case "$mode" in
  primary)
    shift
    if [ "$#" -eq 0 ]; then
      echo "usage: $0 primary isu0 [isu1 isu2 ...]" >&2
      exit 1
    fi
    for name in "$@"; do
      sudo -u postgres psql --no-psqlrc -v ON_ERROR_STOP=1 -qAtX -v slot="$name" <<'EOF'
SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = :'slot';
EOF
      echo "slot $name: dropped"
    done
    ;;
  replica)
    sudo systemctl disable --now postgresql
    echo "postgresql: stopped and disabled"
    ;;
  *)
    echo "usage: $0 primary isu0 | $0 replica" >&2
    exit 1
    ;;
esac
