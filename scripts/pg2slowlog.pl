#!/usr/bin/env perl
# PostgreSQLのjsonlogをMySQLのslow query log形式に変換する
# - auto_explain(log_format=json, log_analyze=on)の実行計画から Rows_sent / Rows_examined / Rows_affected を求める
# - log_lock_waitsの「acquired ... after N ms」を同じプロセスの次のクエリの Lock_time に入れる
# - log_min_duration_statementの「duration: ... statement/execute」も従来どおり読める(行数・ロックは0)
# usage: pg2slowlog.pl /var/log/postgresql/postgresql-slow.json | pt-query-digest
use strict;
use warnings;
use JSON::PP;
use Time::Local qw(timegm);

$| = 1;
my $json = JSON::PP->new;

# pid -> { ms => ロック待ち時間の合計, statement => 待っていたクエリ }
my %lock_waits;

while (my $line = <>) {
    my $log = eval { $json->decode($line) } or next;
    my $message = $log->{message} // next;
    my $pid = $log->{pid} // '';

    if ($message =~ /\Aprocess \d+ acquired .* after ([\d.]+) ms/s) {
        my $wait = $lock_waits{$pid} //= { ms => 0, statement => $log->{statement} // '' };
        $wait->{ms} += $1;
        next;
    }

    my ($duration_ms, $query);
    my ($rows_sent, $rows_examined, $rows_affected) = (0, 0, 0);
    if ($message =~ /\Aduration: ([\d.]+) ms\s+plan:\s*(\{.*\})\s*\z/s) {
        $duration_ms = $1;
        my $explain = eval { $json->decode($2) } or next;
        $query = $explain->{'Query Text'} // next;
        ($rows_sent, $rows_examined, $rows_affected) = plan_rows($explain->{Plan});
    } elsif ($message =~ /\Aduration: ([\d.]+) ms\s+(?:statement|execute [^:]*): (.*)\z/s) {
        # parse/bindは除外し、simple queryのstatementとextended queryのexecuteのみ対象にする
        ($duration_ms, $query) = ($1, $2);
    } else {
        next;
    }

    # ロック待ちは、待っていたクエリと同じものにだけ付ける(LOCK TABLEなどauto_explainに出ない文の分は捨てる)
    my $lock_ms = 0;
    if (my $wait = delete $lock_waits{$pid}) {
        $lock_ms = $wait->{ms} if $wait->{statement} eq $query;
    }

    # slp(pprotein)はMySQLの構文でパースし、失敗したクエリを捨てるためPostgreSQL固有の構文を寄せる
    $query =~ s/\$\d+/?/g;                       # $1 -> ?
    $query =~ s/::\w+(?:\s+precision)?(?:\[\])?//g; # ::timestamptz などのキャストを除去
    $query =~ s/\bFOR NO KEY UPDATE\b/FOR UPDATE/gi;
    $query =~ s/\s*;?\s*\z/;/;

    # pgxの"-- ping"のようなコメントのみのクエリはslpがpanicするため除外する
    (my $body = $query) =~ s{--[^\n]*|/\*.*?\*/}{}gs;
    next unless $body =~ /[^\s;]/;

    my $ts = $log->{timestamp} // '';
    my ($time, $epoch) = ('', 0);
    if ($ts =~ /\A(\d{4})-(\d\d)-(\d\d) (\d\d):(\d\d):(\d\d)(\.\d+)?/) {
        $time  = sprintf('%s-%s-%sT%s:%s:%s%sZ', $1, $2, $3, $4, $5, $6, $7 // '');
        $epoch = timegm($6, $5, $4, $3, $2 - 1, $1);
    }

    my $user = $log->{user} // '';
    my $host = $log->{remote_host} // '';

    print "# Time: $time\n";
    print "# User\@Host: ${user}[${user}] @ ${host} []\n";
    printf "# Query_time: %.6f  Lock_time: %.6f  Rows_sent: %d  Rows_examined: %d  Rows_affected: %d\n",
        $duration_ms / 1000, $lock_ms / 1000, $rows_sent, $rows_examined, $rows_affected;
    print "use $log->{dbname};\n" if defined $log->{dbname};
    print "SET timestamp=$epoch;\n";
    print "$query\n";
}

# 実行計画から(返した行数, 走査した行数, 更新した行数)を求める。
# EXPLAINの行数はループ1回あたりの平均なので、Actual Loopsを掛ける。
sub plan_rows {
    my ($top) = @_;
    return (0, 0, 0) unless ref $top eq 'HASH';

    my $examined = 0;
    my @stack = ($top);
    while (my $node = pop @stack) {
        my $type = $node->{'Node Type'} // '';
        # Bitmap Index ScanはBitmap Heap Scanと二重に数えないよう除外する
        if ($type =~ /Scan\z/ && $type ne 'Bitmap Index Scan') {
            my $per_loop = ($node->{'Actual Rows'} // 0)
                + ($node->{'Rows Removed by Filter'} // 0)
                + ($node->{'Rows Removed by Index Recheck'} // 0);
            $examined += $per_loop * ($node->{'Actual Loops'} // 1);
        }
        push @stack, @{ $node->{Plans} // [] };
    }

    my $loops = $top->{'Actual Loops'} // 1;
    my $sent = ($top->{'Actual Rows'} // 0) * $loops;
    my $affected = 0;
    if (($top->{'Node Type'} // '') eq 'ModifyTable') {
        # INSERT/UPDATE/DELETEは子ノードが渡した行数を更新件数とする(RETURNINGがなければ返す行は0)
        my $child = ($top->{Plans} // [])->[0];
        $affected = ($child->{'Actual Rows'} // 0) * ($child->{'Actual Loops'} // 1) if $child;
    }
    return (int($sent + 0.5), int($examined + 0.5), int($affected + 0.5));
}
