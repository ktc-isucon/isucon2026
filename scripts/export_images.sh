#!/bin/bash
# isu3上で実行する。初期データの画像(initial_data_boundary以前)をDBから
# /home/isucon/images/initial/<id先頭2文字>/<id>.jpg に書き出す(約183万件・13GB)。
# 既に書き出したファイルは飛ばすので、途中で止めても再実行できる。
# usage: bash /home/isucon/scripts/export_images.sh
set -euo pipefail

IMAGE_ROOT=/home/isucon/images
DB_NAME=${ISUREAL_DB_NAME:-isureal}

# nginx(www-data)がPUT/DELETE/読み出しできるようにする
sudo mkdir -p "$IMAGE_ROOT/initial" "$IMAGE_ROOT/runtime"
sudo chown www-data:www-data "$IMAGE_ROOT" "$IMAGE_ROOT/initial" "$IMAGE_ROOT/runtime"
sudo chmod 755 "$IMAGE_ROOT" "$IMAGE_ROOT/initial" "$IMAGE_ROOT/runtime"

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
