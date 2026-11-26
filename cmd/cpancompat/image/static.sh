#!/bin/bash
# One container of the static build phase (cpancompat static). It builds the
# distributions of /app/cpanfile.snapshot in gperl's module directory and,
# for a measured distribution, links a program that loads its modules into
# /bin-out/app, and records what happened under /out:
#
#   phases.jsonl  one object per phase that ran, in order, as dist.sh writes
#                 them: modules (gperl mod download: the snapshot's
#                 distributions built in the module directory, from the
#                 archives the run has checked), link (gperl build of
#                 /app/load.pl into /bin-out/app), and for a target whose
#                 tests run, test_modules (gperl mod download of the test
#                 closure's snapshot) and test_link (the test binary's
#                 project /app/test made, and gperl test -c of it into
#                 /bin-out/app.test)
#   <phase>.log   the output of each phase
#   natives.txt   the Perl packages of the distribution's own native modules
#                 that have a .pm, which the program loads too: their boot
#                 code runs then
#   trees.tsv     the state of each tree of /app/trees once the phases are
#                 over: built (gperl's built mark), failed (its build left
#                 build-error.log), refused (gperl kept its MYMETA.json, which
#                 it copies once the build has succeeded, and marked it
#                 neither way: it refused the native modules the build
#                 made), unbuilt (extracted, none of that), absent
#   trees/<tree>/ what gperl derived from each tree /app/trees marks for
#                 collection, and from each tree that was not built when the
#                 container started: MYMETA.json, the built marks, build-error.log,
#                 and each native module's .so and .ldflags, as they are
#                 below the tree's gperl directory; and archives.txt, the
#                 static archives (lib*.a) in the directories its .ldflags
#                 name with -L
#
# /app holds the program's directory (cpancompat copies it in): the snapshot,
# lib/cpancompat-load.txt (the modules load.pl loads, one per line) and the
# file trees, one "tree<TAB>pathname<TAB>collect" line per distribution of
# the snapshot, the measured distribution's first. For a target whose tests
# run it also holds test-cpanfile.snapshot (the snapshot of the test
# binary's project), test-trees (the trees only the tests need, as in
# trees) and test-files (the test files to run, one per line); /app/test,
# the test binary's project, is the distribution's tree as gperl extracted
# it, with that snapshot.
#
# usage: static.sh app|deps|warm <phase limit in seconds> <own tree or ->
#        static.sh probe|cache
#        static.sh test-dynamic <own tree>
#
# "test-dynamic" makes /app/test and runs the test files on it with gperl
# test, the dynamic interpreter, as the command of its container (no
# phases, no records: cpancompat static runs it like a test binary).
#
# "app" builds the snapshot and links the program, "deps" only builds the
# snapshot (a distribution built because others need it), "warm" links a
# program without dependencies. "probe" records what the image's toolchain
# is instead: search-dirs.txt (the compiler's library directories, in its
# order), archives.txt (the static archives in them), runtime/ (the C
# runtime's shared libraries), versions.json and gperl.json. "cache" links
# a program without dependencies and removes it, leaving the Go build and
# module caches it filled (the container is committed as the image the
# others start from).
#
# It runs as the image's unprivileged user. The orchestration is a shell
# script on purpose, as dist.sh is.
set -u -o pipefail
mode=$1
out=/out
mod=$GPERLMODCACHE
tools=/opt/cpancompat

# test_project <own tree> makes /app/test, the test binary's project: the
# own tree as gperl extracted it, without what gperl derived from it, and
# the snapshot of the test closure.
test_project() {
    mkdir /app/test || return 1
    (cd "$mod/$1" && find . -mindepth 1 -maxdepth 1 ! -name gperl -exec cp -a -t /app/test {} +) || return 1
    cp /app/test-cpanfile.snapshot /app/test/cpanfile.snapshot
}

if [ "$mode" = cache ]; then
    set -e
    work=$(mktemp -d)
    mkdir "$work/lib"
    : > "$work/lib/cpancompat-load.txt"
    cp $tools/load.pl "$work/load.pl"
    (cd "$work" && gperl build -o "$work/app" load.pl)
    rm -rf "$work"
    # The go command sets the modification time of a cache entry it uses
    # when that time is an hour old or more (cmd/go/internal/cache
    # markUsed): in a container started from the committed image, that
    # copies the entry out of the image into the container, all of the
    # cache for every program. Entries dated after any run never are.
    find "$(go env GOCACHE)" -exec touch -d 2100-01-01 {} +
    exit 0
fi

if [ "$mode" = probe ]; then
    set -e
    mkdir -p $out/runtime
    # "libraries: =dir:dir:..." is the format of -print-search-dirs; the list
    # keeps the directories that exist, once each, in order (setup.sh reads
    # it the same way).
    gcc -print-search-dirs | sed -n 's/^libraries: =//p' | tr ':' '\n' |
        while read -r dir; do
            if [ -n "$dir" ] && [ -d "$dir" ]; then (cd "$dir" && pwd); fi
        done | awk '!seen[$0]++' > $out/search-dirs.txt
    while read -r dir; do
        for a in "$dir"/lib*.a; do
            if [ -f "$a" ]; then printf '%s\n' "$a"; fi
        done
    done < $out/search-dirs.txt > $out/archives.txt
    for lib in libc.so.6 libm.so.6 libcrypt.so.1; do
        # A name without a directory is the compiler's way of saying it did
        # not find the file; cp then fails.
        cp -L "$(gcc -print-file-name=$lib)" $out/runtime/$lib
    done
    printf '{"cc":"gcc %s","libc":"%s","go":"%s"}\n' "$(gcc -dumpfullversion)" \
        "$(getconf GNU_LIBC_VERSION)" "$(go env GOVERSION)" > $out/versions.json
    cp $tools/gperl.json $out/gperl.json
    exit 0
fi

if [ "$mode" = modules-step ]; then
    # modules-step <trees file> <project directory>: the archives of the
    # trees go where gperl keeps the archives it downloaded, so that it
    # downloads none; each appears there complete, by a rename, as other
    # containers read them.
    while IFS=$'\t' read -r tree pathname collect; do
        dst=$mod/cache/download/authors/id/$pathname
        [ -f "$dst" ] && continue
        mkdir -p "${dst%/*}" && tmp=$(mktemp "$dst.incoming.XXXXXX") && cp "/archives/$pathname" "$tmp" && mv -f "$tmp" "$dst" || exit 1
    done < "$2"
    cd "$3" && exec gperl mod download
fi

if [ "$mode" = test-link-step ]; then
    # test-link-step <own tree>: the test binary of /app/test.
    test_project "$2" || exit 1
    cd /app/test && exec gperl test -c -o /bin-out/app.test
fi

if [ "$mode" = test-dynamic ]; then
    # The test files, with the environment cpancompat gives the container.
    test_project "$2" || exit 1
    mapfile -t files < /app/test-files || exit 1
    cd /app/test && exec gperl test -j 1 "${files[@]}"
fi

limit=$2
own=$3

# phase <name> <command...> runs one phase with its output in <name>.log and
# appends its record to phases.jsonl.
phase() {
    local name=$1 start=$SECONDS status seconds timed_out=false
    shift
    /usr/bin/timeout --kill-after=10 "$limit" "$@" > "$out/$name.log" 2>&1 < /dev/null
    status=$?
    seconds=$((SECONDS - start))
    # timeout(1) exits 124 when the command ended after its TERM, and is
    # killed itself (137) when it had to send KILL, 10 seconds past the
    # limit (see dist.sh).
    case $status in
        124) timed_out=true ;;
        137) if [ "$seconds" -ge "$limit" ]; then timed_out=true; fi ;;
    esac
    printf '{"phase":"%s","status":%d,"seconds":%d,"timed_out":%s}\n' \
        "$name" "$status" "$seconds" "$timed_out" >> "$out/phases.jsonl"
    return $status
}

# natives lists the packages of the own tree's native modules, as gperl
# names them (the directory of auto/<Module/Path>/<Name>.a), whose .pm is in
# the tree's module directory (gperl's built one when the distribution has a
# build step, else its lib/).
natives() {
    local tree=$mod/$own lib a f rel dir
    lib=$tree/gperl/lib
    [ -d "$lib" ] || lib=$tree/lib
    for a in "$tree"/gperl/abi*/*/auto; do
        [ -d "$a" ] || continue
        find "$a" -type f -name '*.a' | while read -r f; do
            rel=${f#"$a"/}
            dir=${rel%/*}
            if [ -f "$lib/$dir.pm" ]; then printf '%s\n' "${dir//\//::}"; fi
        done
    done | sort -u
}

# collect <tree> copies what gperl derived from the tree into trees/<tree>.
collect() {
    local src=$mod/$1/gperl dst=$out/trees/$1
    [ -d "$src" ] || return 0
    mkdir -p "$dst" || return 1
    (cd "$src" && find . -type f \( -path ./MYMETA.json -o -path ./built -o \
        \( -path './abi*' \( -name built -o -name build-error.log -o -name '*.so' -o -name '*.ldflags' \) \) \) \
        -exec cp --parents -t "$dst" {} +) || return 1
    find "$dst" -name '*.ldflags' -exec sed -n 's|^-L\(/.*\)$|\1|p' {} + | sort -u | while read -r dir; do
        for a in "$dir"/lib*.a; do
            if [ -f "$a" ]; then printf '%s\n' "$a"; fi
        done
    done > "$dst/archives.txt"
}

# state <tree> prints the tree's state in the module directory.
state() {
    local tree=$mod/$1
    if [ ! -d "$tree" ]; then
        echo absent
    elif [ -e "$tree/gperl/built" ] || compgen -G "$tree/gperl/abi*/*/built" > /dev/null; then
        echo built
    elif compgen -G "$tree/gperl/abi*/*/build-error.log" > /dev/null; then
        echo failed
    elif [ -e "$tree/gperl/MYMETA.json" ]; then
        echo refused
    else
        echo unbuilt
    fi
}

mkdir -p $out || exit 1
cp $tools/load.pl /app/load.pl || exit 1
# The trees this container may build: the ones not built yet.
declare -A before
while IFS=$'\t' read -r tree pathname collect; do
    before[$tree]=$(state "$tree") || exit 1
done < /app/trees
link=false
case $mode in
    warm) link=true ;;
    app | deps)
        if phase modules bash "$0" modules-step /app/trees /app && [ "$mode" = app ]; then
            link=true
            if [ "$own" != - ]; then
                natives > $out/natives.txt || exit 1
                cat $out/natives.txt >> /app/lib/cpancompat-load.txt || exit 1
            fi
        fi
        ;;
esac
if $link; then
    # The program's directory is the project: gperl build embeds load.pl and
    # lib/, and links what the snapshot names.
    # The test binary of a target whose program linked: its tests run only
    # once that program has run (cpancompat static). What the test closure
    # needs is built from its snapshot alone; whatever fails on the way is a
    # phase's failure, which leaves the records of the program as they are.
    if phase link bash -c 'cd /app && exec gperl build -o /bin-out/app load.pl' && [ -f /app/test-files ]; then
        phase test_modules bash -c 'mkdir /app/test-modules && cp /app/test-cpanfile.snapshot /app/test-modules/cpanfile.snapshot &&
            exec bash "$0" modules-step /app/test-trees /app/test-modules' "$0" &&
            phase test_link bash "$0" test-link-step "$own"
    fi
fi
while IFS=$'\t' read -r tree pathname collect; do
    if [ "$collect" = 1 ] || [ "${before[$tree]}" != built ]; then collect "$tree" || exit 1; fi
    printf '%s\t%s\n' "$tree" "$(state "$tree")"
done < /app/trees > $out/trees.tsv || exit 1
exit 0
