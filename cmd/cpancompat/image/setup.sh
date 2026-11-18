#!/bin/sh
# Runs once while the image is built, as the user distributions are tested
# as (user.sh): makes gperl the perl command of the machine, the way
# `gperl xs build` arranges it for the build of one distribution.
set -e
G=/opt/gperl
mkdir -p $G/proj $G/fakearchlib $G/local/lib/perl5 $G/local/bin

# The perl command is a program, not a shell script: a shell in between would
# change what the program sees, as sh sets PWD to the working directory when
# PWD does not name it, where perl leaves the environment as it was given.
cat > $G/perl.c <<'EOF'
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

int main(int argc, char **argv) {
    char **args = calloc(argc + 2, sizeof *args);
    if (args == NULL) {
        perror("perl");
        return 127;
    }
    args[0] = "/opt/gperl/bin/gperl";
    args[1] = "run";
    for (int i = 1; i < argc; i++) {
        args[i + 1] = argv[i];
    }
    if (setenv("GPERL_INTERNAL_PERL_CLI", "1", 1) != 0) {
        perror("perl");
        return 127;
    }
    execv(args[0], args);
    perror(args[0]);
    return 127;
}
EOF
cc -O2 -o $G/bin/perl $G/perl.c
rm $G/perl.c

# The XS SDK (the headers native extensions compile against) is unpacked by
# any `gperl xs build`; the distribution named here does not exist.
$G/bin/gperl xs build -C $G/proj /nonexistent >/dev/null 2>&1 || true
test -f $G/proj/local/xs/.sdk/perl.h
ln -s $G/proj/local/xs/.sdk $G/fakearchlib/CORE

# The standard library is unpacked into the user's cache on first use; that
# directory is what %Config has to name, because it is the one in @INC.
stdlib=$($G/bin/perl -e 'require strict; my $p = $INC{"strict.pm"}; $p =~ s{/strict\.pm\z}{}; print $p')
test -f "$stdlib/strict.pm"
# The SDK has a config.h, so the Makefile ExtUtils::MakeMaker writes depends
# on Config.pm in the architecture library.
ln -s "$stdlib/Config.pm" $G/fakearchlib/Config.pm

# perl's programs (prove, perldoc, pod2usage, ...) are beside the path that
# runs gperl as the perl command, which %Config names as bin and scriptdir
# (the directory of $^X). Here, before GPERL_PERL_EXE names the shim, that is
# gperl's own directory for them; after it, the shim's directory has to hold
# them too.
scriptdir=$($G/bin/perl -MConfig -e 'print $Config{scriptdir}')
test -f "$scriptdir/prove"
for f in "$scriptdir"/*; do
    [ "${f##*/}" = perl ] || ln -sf "$f" $G/bin/
done

# The compiler %Config describes is this machine's, in every key that names
# it, as `gperl xs build` describes it (hostToolchain in gperl/xs.go): build
# scripts choose flags by the compiler's identity, pass the preprocessor
# flags and libraries on to the C configure scripts they run, and look for
# libraries where the compiler looks. The interpreter's own values describe
# the wasm32 toolchain it was built with.
ccversion=$(cc -dumpfullversion)
# "libraries: =dir:dir:..." is the format of -print-search-dirs; the list
# keeps the directories that exist, once each, in order.
libpth=$(cc -print-search-dirs | sed -n 's/^libraries: =//p' | tr ':' '\n' |
    while read -r dir; do
        [ -n "$dir" ] && [ -d "$dir" ] && (cd "$dir" && pwd)
    done | awk '!seen[$0]++' | tr '\n' ' ' | sed 's/ $//')
test -n "$libpth"
# A name without a directory is the compiler's way of saying it did not
# find the file.
libc=$(cc -print-file-name=libc.so.6)
case $libc in
/*) test -e "$libc" || libc= ;;
*) libc= ;;
esac
glibc=$(printf '#include <features.h>\n__GLIBC__ __GLIBC_MINOR__\n' | cc -E -P -x c - | tail -1 | tr ' ' .)

sed -e "s|@STDLIB@|$stdlib|g" -e "s|@CCVERSION@|$ccversion|g" \
    -e "s|@LIBPTH@|$libpth|g" -e "s|@LIBC@|$libc|g" -e "s|@GLIBC@|$glibc|g" \
    > $G/config-overlay.pl <<'EOF'
my $ccflags = '-fno-strict-aliasing -fstack-protector-strong -DPERL_USE_SAFE_PUTENV -D_GNU_SOURCE -I/opt/gperl/proj/local/xs/.sdk';
# Config.pm reports these in place of its own values in every view of
# %Config (the hash, config_sh, perl -V); they are given before it is loaded,
# so that a program starts with no module loaded, as on perl.
%Config::Override = (
    'perlpath' => '/opt/gperl/bin/perl', 'startperl' => '#!/opt/gperl/bin/perl',
    'cc' => 'cc', 'ld' => 'cc', 'ar' => 'ar', 'full_ar' => 'ar',
    'ranlib' => 'ranlib', 'make' => 'make',
    'ccname' => 'gcc', 'gccversion' => '@CCVERSION@', 'ccversion' => '',
    'gccosandvers' => '',
    'cpp' => 'cc -E', 'cpprun' => 'cc -E', 'cppstdin' => 'cc -E',
    'cppminus' => '-', 'cpplast' => '-', 'cppflags' => $ccflags,
    'ccsymbols' => '', 'cppsymbols' => '', 'cppccsymbols' => '',
    'libc' => '@LIBC@', 'gnulibc_version' => '@GLIBC@',
    'libs' => '-lm', 'perllibs' => '-lm',
    'libpth' => '@LIBPTH@', 'glibpth' => '@LIBPTH@', 'libspath' => '@LIBPTH@',
    'ccflags' => $ccflags,
    'optimize' => '-O2', 'ldflags' => '', 'cccdlflags' => '-fPIC',
    'ccdlflags' => '', 'obj_ext' => '.o', 'exe_ext' => '', 'lib_ext' => '.a',
    'usedl' => 'define', 'dlsrc' => 'dl_dlopen.xs', 'useshrplib' => 'false',
    'so' => 'so', 'dlext' => 'so', 'lddlflags' => '-shared',
    'privlibexp' => '@STDLIB@', 'privlib' => '@STDLIB@',
    'archlibexp' => '/opt/gperl/fakearchlib', 'archlib' => '/opt/gperl/fakearchlib',
    'vendorlibexp' => '', 'vendorarchexp' => '',
    'installprivlib' => '@STDLIB@', 'installarchlib' => '/opt/gperl/fakearchlib',
);
1;
EOF

GPERL_PERL_PRELOAD=$G/config-overlay.pl $G/bin/perl -MConfig -e 'print "perl $^V as $^X, cc=$Config{cc}, osname=$^O\n"'
# $^O and $Config{osname} are gperl's own (the system it runs on); the image
# only checks them, so the survey measures what a user of this gperl gets.
GPERL_PERL_PRELOAD=$G/config-overlay.pl $G/bin/perl -MConfig -e 'exit($^O eq "linux" && $Config{osname} eq "linux" ? 0 : 1)' || {
    echo "setup: this gperl reports \$^O as something other than linux" >&2
    exit 1
}
