# The program of the static build phase (static.sh): it loads each module
# named in lib/cpancompat-load.txt beside it, one name per line, and exits 1
# when one fails to load, naming it and the error on standard error. A
# module is loaded by its file name, the way require loads a bareword, not
# through a string eval.
use strict;
use warnings;

my $dir = __FILE__;
$dir =~ s{[^/]*\z}{};
my $list = "${dir}lib/cpancompat-load.txt";
open my $fh, '<', $list or die "load.pl: $list: $!\n";
my $failed = 0;
while (my $module = <$fh>) {
    chomp $module;
    next if $module eq '';
    (my $file = "$module.pm") =~ s{::}{/}g;
    next if eval { require $file; 1 };
    print STDERR "load.pl: $module: $@";
    $failed = 1;
}
close $fh or die "load.pl: $list: $!\n";
exit $failed;
