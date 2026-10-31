#!/bin/bash
# 最終計測用の切り替え。手元(リポジトリの scripts/)から実行し、ssh で各サーバーの設定を変える。
#   on : 計測用のログ・プロファイラを止める(最終計測の前)
#   off: 元に戻して計測を再開する
#   status: 今の状態を表示する
# 何度実行しても同じ状態になる。
#
# usage: bash scripts/final_mode.sh on|off|status
#
# 変えるもの
#   isu3 : PostgreSQL の auto_explain(log_min_duration=-1)、log_lock_waits=off、deadlock_timeout=1s、
#          log_destination=stderr(jsonlog を止める)、pg_stat_statements.track=none
#          → ALTER SYSTEM(postgresql.auto.conf)で上書きするので、push:all の postgresql.conf の配置では消えない。off は ALTER SYSTEM RESET。
#          pg2slowlog.service と pprotein-agent.service を止めて自動起動も外す
#   isu0〜2: nginx の access_log を off、env.sh の PPROTEIN_URL を空(元の値は保存して off で戻す)、
#          ISUREAL_DISABLE_DEBUG_SERVER=1 でアプリ内の pprotein デバッグサーバー(:8383)を止める、アプリを再起動
#   全台 : /home/isucon/.final_mode を置く(taskfile の restart:pprotein がこれを見て pprotein を起動しない)
#
# 注意: task push:all を流すと nginx.conf が通常版(access_log あり)に戻る。push:all のあとは on をもう一度実行すること。
set -euo pipefail

APP_SERVERS=(isu0 isu1 isu2)
PG_SERVER=isu3
MARKER=/home/isucon/.final_mode
ACCESS_LOG_LINE='access_log /var/log/nginx/access.log ltsv buffer=64k flush=1s;'

mode="${1:-}"
case "$mode" in
  on|off|status) ;;
  *) echo "usage: $0 on|off|status" >&2; exit 1 ;;
esac

remote() {
  local host=$1
  shift
  ssh -o BatchMode=yes "$host" "$@" 2>&1 | grep -v -i -E "locale|LANG|LC_|are supported|Falling back" || true
}

pg_on() {
  remote "$PG_SERVER" "sudo -u postgres psql --no-psqlrc -qAtX -v ON_ERROR_STOP=1 \
    -c \"ALTER SYSTEM SET auto_explain.log_min_duration = -1\" \
    -c \"ALTER SYSTEM SET log_lock_waits = off\" \
    -c \"ALTER SYSTEM SET deadlock_timeout = '1s'\" \
    -c \"ALTER SYSTEM SET log_destination = 'stderr'\" \
    -c \"ALTER SYSTEM SET pg_stat_statements.track = 'none'\" \
    -c \"SELECT pg_reload_conf()\" >/dev/null && echo 'isu3 postgresql: final settings applied'"
  remote "$PG_SERVER" "sudo systemctl disable --now pg2slowlog.service pprotein-agent.service >/dev/null 2>&1; touch $MARKER; echo 'isu3 pg2slowlog/pprotein-agent: stopped'"
}

pg_off() {
  remote "$PG_SERVER" "rm -f $MARKER; sudo -u postgres psql --no-psqlrc -qAtX -v ON_ERROR_STOP=1 \
    -c \"ALTER SYSTEM RESET auto_explain.log_min_duration\" \
    -c \"ALTER SYSTEM RESET log_lock_waits\" \
    -c \"ALTER SYSTEM RESET deadlock_timeout\" \
    -c \"ALTER SYSTEM RESET log_destination\" \
    -c \"ALTER SYSTEM RESET pg_stat_statements.track\" \
    -c \"SELECT pg_reload_conf()\" >/dev/null && echo 'isu3 postgresql: settings restored'"
  remote "$PG_SERVER" "sudo systemctl enable --now pg2slowlog.service pprotein-agent.service >/dev/null 2>&1; echo 'isu3 pg2slowlog/pprotein-agent: started'"
}

# env.sh の PPROTEIN_URL を空にし、元の値は「#final_mode_saved PPROTEIN_URL=...」として残す
app_on() {
  local host=$1
  remote "$host" "set -e
    f=/home/isucon/env.sh
    if grep -q '^PPROTEIN_URL=..*' \$f; then
      sed -i 's|^PPROTEIN_URL=\(..*\)\$|#final_mode_saved PPROTEIN_URL=\1\nPPROTEIN_URL=|' \$f
    fi
    grep -q '^ISUREAL_DISABLE_DEBUG_SERVER=1' \$f || echo 'ISUREAL_DISABLE_DEBUG_SERVER=1' >> \$f
    sudo sed -i 's|^\(\s*\)access_log /var/log/nginx/access.log ltsv[^;]*;|\1access_log off; # final_mode|' /etc/nginx/nginx.conf
    sudo nginx -t -q && sudo systemctl reload nginx
    touch $MARKER
    sudo systemctl restart isureal-go
    echo \"\$(hostname): final mode on\""
}

app_off() {
  local host=$1
  remote "$host" "set -e
    f=/home/isucon/env.sh
    if grep -q '^#final_mode_saved PPROTEIN_URL=' \$f; then
      sed -i '/^PPROTEIN_URL=\$/d' \$f
      sed -i 's|^#final_mode_saved PPROTEIN_URL=|PPROTEIN_URL=|' \$f
    fi
    sed -i '/^ISUREAL_DISABLE_DEBUG_SERVER=1\$/d' \$f
    sudo sed -i 's|^\(\s*\)access_log off; # final_mode|\1$ACCESS_LOG_LINE|' /etc/nginx/nginx.conf
    sudo nginx -t -q && sudo systemctl reload nginx
    rm -f $MARKER
    sudo systemctl restart isureal-go
    echo \"\$(hostname): final mode off\""
}

status() {
  for host in "${APP_SERVERS[@]}"; do
    remote "$host" "printf '%s marker=%s ' \$(hostname) \$(test -e $MARKER && echo yes || echo no)
      printf 'pprotein_url=%s ' \"\$(grep '^PPROTEIN_URL=' /home/isucon/env.sh | cut -d= -f2-)\"
      printf 'debug_disabled=%s ' \$(grep -c '^ISUREAL_DISABLE_DEBUG_SERVER=1' /home/isucon/env.sh)
      printf 'access_log=%s\n' \"\$(sudo grep -E '^\s*access_log' /etc/nginx/nginx.conf | sed 's/^\s*//')\""
  done
  remote "$PG_SERVER" "printf '%s marker=%s ' \$(hostname) \$(test -e $MARKER && echo yes || echo no)
    printf 'pg2slowlog=%s pprotein-agent=%s\n' \$(systemctl is-active pg2slowlog) \$(systemctl is-active pprotein-agent)
    sudo -u postgres psql --no-psqlrc -qAtX -c \"SELECT name || '=' || setting FROM pg_settings WHERE name IN ('auto_explain.log_min_duration','log_lock_waits','deadlock_timeout','log_destination','pg_stat_statements.track') ORDER BY name\""
}

case "$mode" in
  on)
    pg_on
    for host in "${APP_SERVERS[@]}"; do app_on "$host"; done
    status
    ;;
  off)
    for host in "${APP_SERVERS[@]}"; do app_off "$host"; done
    pg_off
    status
    ;;
  status)
    status
    ;;
esac
