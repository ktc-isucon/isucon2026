#!/bin/bash
# 初期データの画像(initial_data_boundary以前)を、このサーバーのDBから
# $IMAGE_ROOT/initial/<id先頭2文字>/<id>.jpg に書き出す(約183万件・13GB)。
# 既に書き出したファイルは飛ばすので、途中で止めても再実行できる。
# usage:
#   isu3(画像の置き場所):       bash /home/isucon/scripts/export_images.sh
#   isu0〜2(nginxのキャッシュ): bash /home/isucon/scripts/export_images_local.sh
set -euo pipefail

IMAGE_ROOT=${IMAGE_ROOT:-/home/isucon/images}
# isu3ではアプリがPUT/DELETEする runtime/ も用意する
WITH_RUNTIME=${WITH_RUNTIME:-1}
DB_NAME=${ISUREAL_DB_NAME:-isureal}

# nginx(www-data)が読み書きできるようにする
dirs=("$IMAGE_ROOT" "$IMAGE_ROOT/initial")
if [ "$WITH_RUNTIME" = 1 ]; then
  dirs+=("$IMAGE_ROOT/runtime")
fi
sudo mkdir -p "${dirs[@]}"
sudo chown www-data:www-data "${dirs[@]}"
sudo chmod 755 "${dirs[@]}"

sudo -u postgres psql --no-psqlrc -d "$DB_NAME" -c "
  COPY (
    SELECT id, encode(data, 'hex')
    FROM image_objects
    WHERE created_at <= (SELECT created_at FROM initial_data_boundary)
  ) TO STDOUT
" | sudo -u www-data perl -e '
  use strict;
  use warnings;
  my $root = shift;
  my ($written, $skipped) = (0, 0);
  while (my $line = <STDIN>) {
    chomp $line;
    my ($id, $hex) = split /\t/, $line, 2;
    next unless defined $hex && $id =~ /\A[0-9a-f-]{36}\z/;
    my $dir = "$root/" . substr($id, 0, 2);
    my $path = "$dir/$id.jpg";
    if (-e $path) {
      $skipped++;
      next;
    }
    mkdir $dir unless -d $dir;
    open my $fh, ">:raw", "$path.tmp" or die "open $path.tmp: $!";
    print {$fh} pack("H*", $hex) or die "write $path.tmp: $!";
    close $fh or die "close $path.tmp: $!";
    rename "$path.tmp", $path or die "rename $path: $!";
    $written++;
    print STDERR "written=$written skipped=$skipped\n" if ($written + $skipped) % 100000 == 0;
  }
  print STDERR "done: written=$written skipped=$skipped\n";
' "$IMAGE_ROOT/initial"
