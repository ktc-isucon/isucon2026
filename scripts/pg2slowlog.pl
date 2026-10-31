#!/usr/bin/env perl
# PostgreSQLのjsonlog(log_min_duration_statement)をMySQLのslow query log形式に変換する
# usage: pg2slowlog.pl /var/log/postgresql/postgresql-slow.json | pt-query-digest
use strict;
use warnings;
use JSON::PP;
use Time::Local qw(timegm);

$| = 1;
my $json = JSON::PP->new;

while (my $line = <>) {
    my $log = eval { $json->decode($line) } or next;
    my $message = $log->{message} // next;

    # parse/bindは除外し、simple queryのstatementとextended queryのexecuteのみ対象にする
    next unless $message =~ /\Aduration: ([\d.]+) ms\s+(?:statement|execute [^:]*): (.*)\z/s;
    my ($duration_ms, $query) = ($1, $2);

    $query =~ s/\s*;?\s*\z/;/;

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
    printf "# Query_time: %.6f  Lock_time: 0.000000  Rows_sent: 0  Rows_examined: 0\n", $duration_ms / 1000;
    print "use $log->{dbname};\n" if defined $log->{dbname};
    print "SET timestamp=$epoch;\n";
    print "$query\n";
}
