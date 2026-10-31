#!/bin/bash
# isu3(primary)上で実行する。image_objects の画像本体(data)を空にして DB を小さくする。
# 画像本体は /home/isucon/images(isu3)と各サーバーのキャッシュから配っており、アプリは data 列を読まない。
#
# UPDATE で data を空にすると、TOAST の各ページの写しが WAL に書かれて十数GBに膨らむため、
# パーティションごとに「data を空にしたコピーを作り、古いパーティションと入れ替える」方式にする。
# WAL に書かれるのはコピーした小さな行だけで、古いパーティションの DROP はほとんど WAL を出さない。
# 各パーティションは1トランザクションで入れ替えるので、途中で止めても壊れない(再実行すると続きから進む)。
#
# usage: bash /home/isucon/scripts/shrink_image_objects.sh [DB名]
set -euo pipefail

DB_NAME=${1:-isureal}

psql_db() {
  sudo -u postgres psql --no-psqlrc -v ON_ERROR_STOP=1 -qAtX -d "$DB_NAME" "$@"
}

echo "== before: $(psql_db -c "SELECT pg_size_pretty(pg_database_size(current_database()))")"

# data が空でない行を持つパーティションだけを対象にする(再実行したときは済んだものを飛ばす)
psql_db -F $'\t' -c "
  SELECT c.relname, pg_get_expr(c.relpartbound, c.oid)
  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
  WHERE i.inhparent = 'image_objects'::regclass
  ORDER BY c.relname" |
while IFS=$'\t' read -r part bound; do
  remaining=$(psql_db -c "SELECT count(*) FROM (SELECT 1 FROM $part WHERE length(data) > 0 LIMIT 1) s")
  if [ "$remaining" = "0" ]; then
    echo "skip $part (already empty)"
    continue
  fi
  start=$(date +%s)
  psql_db -1 <<EOF
CREATE TABLE ${part}_shrink (LIKE $part INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING STORAGE);
INSERT INTO ${part}_shrink (id, owner_user_id, data, media_type, created_at, origin_region)
  SELECT id, owner_user_id, ''::bytea, media_type, created_at, origin_region FROM $part;
ALTER TABLE image_objects DETACH PARTITION $part;
DROP TABLE $part;
ALTER TABLE ${part}_shrink RENAME TO $part;
ALTER TABLE image_objects ATTACH PARTITION $part $bound;
EOF
  echo "done $part ($(( $(date +%s) - start ))s)"
done

psql_db -c "ANALYZE image_objects"
echo "== after: $(psql_db -c "SELECT pg_size_pretty(pg_database_size(current_database()))")"
