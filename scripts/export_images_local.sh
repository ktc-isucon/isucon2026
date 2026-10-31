#!/bin/bash
# isu0〜2上で実行する。止めてある手元のPostgreSQL(isu3と同じ初期データ入り)を一時的に起動し、
# 初期データの画像をnginxのキャッシュ(/home/isucon/image-cache/_img/initial/)に書き出してから止める。
# ネットワークを使わないので速く、ルーターを経由しない通信も発生しない。
# ベンチマークを流していないときに実行すること。
# usage: bash /home/isucon/scripts/export_images_local.sh
set -euo pipefail

cd "$(dirname "$0")"

sudo systemctl start postgresql
trap 'sudo systemctl stop postgresql' EXIT

IMAGE_ROOT=/home/isucon/image-cache/_img WITH_RUNTIME=0 bash ./export_images.sh
