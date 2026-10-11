#!/usr/bin/env perl
# e2e-checks.pl LINES < source.go — used by scripts/e2e-new-checks.sh.
#
# Prints "<line>\t<pattern>" for every check call in the Go source on stdin
# that has one of LINES (space-separated line numbers, or "all") anywhere in
# it: the line the call starts on, and the regexp of its check name as
# `go test` matches it (spaces as underscores).
#
#   h.check(t, NAME, …) / h.assert(t, NAME, …) / h.fail(t, NAME, …)
#       NAME's pattern, anchored by the caller: literal parts escaped,
#       fmt.Sprintf verbs and non-literal operands as .*
#   h.<helper>(t, …, "string") / f.<helper>(t, …, "string")
#       a helper that names its checks after its last argument
#       (objectCacheSet, warmSite, ensureObjectCache): .*string.*
use strict;
use warnings;

my %want = map { $_ => 1 } split / /, ($ARGV[0] // "");
my $all  = ($ARGV[0] // "") eq "all";
my $src  = do { local $/; <STDIN> } // "";
my $n    = length $src;
my $str  = qr/"(?:[^"\\\n]|\\.)*"|`[^`]*`/s;

# arg(pos): the argument that starts at pos, up to its top-level "," or the
# call's ")": (text, position of that "," or ")").
sub arg {
    my ($pos) = @_;
    my ($depth, $start) = (0, $pos);
    while ($pos < $n) {
        pos($src) = $pos;
        if ($src =~ /\G($str)/gc) { $pos = pos($src); next }
        if ($src =~ m{\G//[^\n]*}gc) { $pos = pos($src); next }
        my $c = substr($src, $pos, 1);
        if ($c =~ /[(\[{]/) {
            $depth++;
        } elsif ($c =~ /[)\]}]/) {
            return (substr($src, $start, $pos - $start), $pos) if $depth == 0;
            $depth--;
        } elsif ($c eq "," && $depth == 0) {
            return (substr($src, $start, $pos - $start), $pos);
        }
        $pos++;
    }
    return (substr($src, $start), $pos);
}

sub unquote {
    my ($s) = @_;
    return substr($s, 1, -1) if $s =~ /\A`/;
    $s = substr($s, 1, -1);
    $s =~ s/\\(.)/$1 eq "n" ? "\n" : $1 eq "t" ? "\t" : $1/ge;
    return $s;
}

sub esc {
    my ($s) = @_;
    $s =~ s/([\\.+*?()|\[\]{}^\$])/\\$1/g;
    $s =~ s/\s/_/g;
    return $s;
}

sub trim { my ($s) = @_; $s =~ s/\A\s+|\s+\z//g; return $s }

# name_pattern: the regexp of a check-name expression.
sub name_pattern {
    my $e = trim($_[0]);
    my $p;
    if ($e =~ /\Afmt\.Sprintf\(\s*($str)/) {
        $p = join "", map { $_ eq "%%" ? "%" : /\A%/ ? ".*" : esc($_) }
            grep { length } split /(%%|%[-+# 0-9.*\[\]]*[a-zA-Z])/, unquote($1);
    } else {
        my @ops;
        my ($cur, $depth) = ("", 0);
        my $rest = $e;
        while (length $rest) {
            if ($rest =~ s/\A($str)//) { $cur .= $1; next }
            my $c = substr($rest, 0, 1, "");
            if    ($c =~ /[(\[{]/) { $depth++ }
            elsif ($c =~ /[)\]}]/) { $depth-- }
            if ($c eq "+" && $depth == 0) { push @ops, $cur; $cur = ""; next }
            $cur .= $c;
        }
        push @ops, $cur;
        $p = join "", map { my $o = trim($_); $o =~ /\A$str\z/ ? esc(unquote($o)) : ".*" } @ops;
    }
    $p =~ s/(?:\.\*)+/.*/g;
    # `go test` splits a test's name at each "/" and matches the third
    # pattern element against the part before the first one. A literal "/"
    # ends the pattern there; so does the first run-time part, which may hold
    # a "/" too (a path, say): it becomes a prefix match.
    my $slash = index($p, "/");
    my $any   = index($p, ".*");
    if ($slash >= 0 && ($any < 0 || $slash < $any)) {
        $p = substr($p, 0, $slash);
    } elsif ($any >= 0) {
        $p = substr($p, 0, $any + 2);
    }
    return $p;
}

sub line_at { return 1 + (substr($src, 0, $_[0]) =~ tr/\n//) }

my %skip = map { $_ => 1 } qw(Run Log Logf Error Errorf Fatal Fatalf Skip Skipf Helper);
pos($src) = 0;
while ($src =~ /\b(?:f\.h|h|f)\.(\w+)\(\s*t\s*,/g) {
    my ($method, $after, $start) = ($1, pos($src), $-[0]);
    my (@args, $end);
    my $p = $after;
    while (1) {
        (my $a, $end) = arg($p);
        push @args, $a;
        last if $end >= $n || substr($src, $end, 1) eq ")";
        $p = $end + 1;
    }
    pos($src) = $after;
    my ($l0, $l1) = (line_at($start), line_at($end));
    my $touched = $all;
    for my $l ($l0 .. $l1) { $touched ||= $want{$l} }
    next unless $touched;
    if ($method =~ /\A(?:check|assert|fail)\z/) {
        print "$l0\t", name_pattern($args[0]), "\n";
    } elsif (!$skip{$method} && @args && trim($args[-1]) =~ /\A($str)\z/) {
        my $s = unquote($1);
        print "$l0\t.*", esc($s), ".*\n" if $s =~ /[A-Za-z]/ && $s !~ /\n/;
    }
}
