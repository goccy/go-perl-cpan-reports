# go-perl CPAN Testers

How much of CPAN passes its own tests on [go-perl](https://github.com/goccy/go-perl),
measured the way [CPAN Testers](https://www.cpantesters.org/) measure perl: every
distribution is configured, built, tested and installed as a CPAN client does it, with
everything it needs installed the same way first. The results are kept in this
repository, and a static site generated from them is published on GitHub Pages.

The set measured against is the **reference set**: the latest CPAN releases whose
CPAN Testers reports on the perl version go-perl is built from (perl 5.44.0) pass more
often than they fail on Linux. The same distributions are run on perl 5.44.0 itself,
in the same containers and harness, so that what only go-perl fails stands apart from
what fails in this environment anyway.

## Current results

The results in `results/` are **provisional**. The go-perl results are not one run of
one binary: `gperl-7647471-combined` gives each distribution its latest result from the
development builds of go-perl measured from 2026-11-17 to 2026-11-19, on unreleased
branches. 2,165 distributions were last measured with go-perl `7647471` or `49e6b7c`
(which differs from it in one narrow fix), 7,952 come from the full survey of
`ee7dd1b`, and the rest from a partial survey of `58ab759` and spot checks of other
builds; the run's `meta.json` gives the breakdown. They will be replaced by one full
survey of gperl once these changes are merged.

| run | interpreter | passes, of 14,318 |
|---|---|---|
| `gperl-7647471-combined` | gperl, development builds up to go-perl `7647471` | 13,487 (94.2%) |
| `ref2` | perl 5.44.0 | 13,981 (97.6%) |

go-perl passes 96.3% of what perl passes. All runs: the CPAN snapshot of 2026-11-10,
16 distributions at a time, each container with one CPU and 1500 MiB of memory, the
test phase of a distribution limited to five minutes, on linux/arm64.

## Repository layout

```
cmd/cpancompat/          the survey tool (one command, cpancompat)
cmd/cpancompat/image/    the files of the container images distributions are tested in
cmd/cpancompat/site/     the templates and assets of the site
cmd/cpancompat/known-causes.json
                         causes found by analysis where the logs cannot tell them
                         (see "Known causes")
results/                 the results the site is generated from
.github/workflows/       the workflow that publishes the site
Makefile                 a survey from start to end
```

`cpancompat` works in steps, each reading and writing plain files in a data
directory, so every step can be run again on its own:

| step | what it does |
|---|---|
| `fetch` | downloads the CPAN package index and the MetaCPAN metadata of every latest release |
| `testers` | downloads the CPAN Testers reports made on one perl version |
| `graph` | joins them into one record per distribution and derives the dependency graph |
| `image` | builds the image distributions are tested in, around gperl or an official perl image |
| `run` | configures, builds, tests and installs the distributions, one container each, in dependency order |
| `static` | for each distribution that passed in a run, builds a program that loads it into a fully static binary with gperl, and runs the binary in a container that holds nothing else (`image -static` makes its image) |
| `report` | why each distribution that did not pass failed, and how a run compares with a run of perl |
| `refset`, `export` | write the snapshot, the reference set and a run into `results/` |
| `site` | generates the site from `results/` |

`cpancompat` without arguments lists every option.

### The results

```
results/snapshot.json           the CPAN snapshot and the size of the reference set
results/reference.jsonl.gz      the reference set: distribution, release, CPAN Testers
                                Linux pass and fail counts on perl 5.44.0, dependents
results/packages.jsonl.gz       every distribution of the snapshot: its modules and their
                                versions in the package index, and the distributions its
                                configure, build and runtime requirements name
results/<run>/meta.json         the interpreter and its commit, whether the run is
                                provisional, the image, the dates, and the limits and
                                harness of each invocation of `run`; the network policy;
                                how the static build phase was made, when there was one
results/<run>/results.jsonl.gz  the result of each distribution: outcome, phases, test files
results/<run>/failures.jsonl.gz why each distribution that did not pass failed: failing
                                phase and test files, evidence lines, cause, blockers
results/<run>/logs.jsonl.gz     the end of the failing phase's log of each of them
results/<run>/provided.json     the modules the interpreter ships, with their versions
results/<run>/static.jsonl.gz   the static build of each distribution that passed: state,
                                reason, the libraries of the system it links, its binary
results/<run>/static-logs.jsonl.gz
                                the end of the log of each static build that failed
```

The site's cpanfile checker (`check.html`) reads `check.json`, which `site` makes from
`packages.jsonl.gz` and the subject run's results, static builds and `provided.json`.

The files are gzip-compressed JSON lines. The records hold no path of the machine a
run was made on: `export` replaces the data directory's path and cuts paths below the
home directory short.

### Known causes

`report` reads the cause of each failure from the log of its failing phase, by the
patterns of the messages in it. One cause is a known limitation of go-perl. gperl runs a
`fork` child as a pseudo-process, the way perl does on Windows: the interpreter's memory
is copied and the child runs on a goroutine of the same OS process. The memory native
(XS) modules allocate in C is not part of the interpreter's memory and is not copied (DBI
handles' C structs, SSL contexts, libev loops, the modules' own objects), so native code
is not available in a forked child: calling an XSUB, loading a native module or running
a hook one installed dies with a Perl error such as

```
Foo::bar: native modules cannot be used in a forked child (.../auto/Foo/Foo.so) at script.pl line 12.
```

perl itself, with a real fork, and pure-Perl code are not affected. A failure whose
evidence lines carry that message, and no crash, takes the cause
`fork-child-native-code`, and so does a phase that ran out of time where the test file
running at the limit printed it (its child died and it waited for the child). A child whose output was discarded (a daemon that closes its
standard error, a server child whose messages go nowhere) leaves no message in the log,
and the failure shows only as a parent that waited in vain or a test that saw a closed
connection. Those failures are attributed by analysis and recorded as data in
`cmd/cpancompat/known-causes.json`, a JSON array with one entry per distribution:

```json
{
  "dist": "Some-Server",
  "cause": "fork-child-native-code",
  "tests": ["t/server.t"],
  "evidence": "The server child loads JSON::XS after fork with its standard error closed; run with it open, it dies with go-perl's message, and the client sees the connection closed."
}
```

| field | what it holds |
|---|---|
| `dist` | the distribution's name; one entry per distribution |
| `cause` | the cause id: at present only `fork-child-native-code` is accepted |
| `tests` | the test files the analysis attributed to the cause |
| `evidence` | what ties them to it, in a sentence or two: which native module the child uses, and how the analysis saw the message the log lacks |

The file is built into `cpancompat`. `report -interpreter-name go-perl`, `export
-interpreter-name go-perl` and `site` (for a run exported as go-perl) give a
distribution its entry's cause only when its tests failed (outcome `fail`) and every
test file that failed in the run is among the entry's `tests`. Otherwise the cause its
log tells stands: a distribution that also failed in another test file, ran out of time,
did not build or was blocked is never relabelled, and neither is any run of perl. `site`
applies the file to the runs it reads, so an entry added after a run was exported shows
on the site at once; `export` writes the cause into the records, so after an entry is
narrowed or removed, export the run again.

Justify every entry from the survey's records and logs, or from a reproduction:

- List a test file only when go-perl's message is shown to be what failed it: run the
  test in the survey image with the child's output kept (for example with the server's
  log or standard error sent to a file) and find the message there.
- Leave out the test files that fail for another reason, even in the same distribution,
  and the distributions of which no test file can be tied.
- Write the evidence in the entry: the native module the child uses and how the message
  was seen. Keep it free of paths of your machine and of anything that only means
  something in your own notes.

## Viewing the site locally

```sh
make serve
```

(or `go run ./cmd/cpancompat site -results results -out _site -serve localhost:8000`)
writes the site to `_site/` and serves it at <http://localhost:8000/>.

## Reproducing a survey

A survey runs on your own machine, in Docker; its results are then committed. One
command runs it from start to end:

```sh
make survey GPERL=path/to/gperl GPERL_COMMIT=<the go-perl commit gperl was built from>
```

### Prerequisites

- **Go** (the version in `go.mod`), **make** and the **Docker** command line.
- **A Docker host of its own**: a virtual machine with 8 CPUs, 16 GB of memory and a
  60 GB disk, for example a [colima](https://github.com/abiosoft/colima) profile.
  Do not use the Docker host you use for other work. Every distribution runs its own
  code during a survey, and some of it may be malicious; and `run` installs firewall
  rules (iptables) in the Docker host, which keep the containers away from your
  network and from the host. The containers run as an unprivileged user without
  capabilities and get no writable directory of your machine, and the virtual machine
  needs to see only the archives directory, read-only. With colima:

  ```sh
  mkdir -p data/gperl/archives
  colima start --profile cpancompat --cpu 8 --memory 16 --disk 60 --edit
  ```

  and, in the configuration the editor opens, mount the archives directory alone,
  read-only (by its absolute path):

  ```yaml
  mounts:
    - location: /path/to/go-perl-cpan-reports/data/gperl/archives
      writable: false
  ```

  Then point Docker at it for the commands that follow:
  `export DOCKER_CONTEXT=colima-cpancompat`.

- **gperl for Linux**, built for the architecture of the virtual machine (arm64 on
  Apple silicon, amd64 on most other machines) from go-perl at the commit to measure:

  ```sh
  cd go-perl
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o gperl-linux-arm64 ./cmd/gperl
  ```

  `-trimpath` keeps the paths of your machine out of the binary, whose Go stack traces
  in the logs name source files by them.

  The provisional results in this repository were made with gperl built from
  unreleased go-perl development branches (up to commit `7647471`, with interpreter
  builds that are not published), which cannot be rebuilt from public sources. A survey
  of the released gperl, or of any public go-perl commit, can be.

- About 10 GB free on your machine for the data directories: the archives of the
  distributions (3.7 GB), the downloads, and the records and logs of both runs.

### The steps

`make survey` runs these targets in order. Each can be run on its own and again; an
interrupted `run` continues where it stopped.

| target | what it does | takes |
|---|---|---|
| `snapshot` | the CPAN snapshot and the CPAN Testers reports on perl 5.44.0, into `data/gperl` (shared with `data/perl`) | tens of minutes |
| `images` | the image around gperl, and the same environment around `perl:5.44.0-slim-bookworm` | tens of minutes the first time |
| `provided` | the modules each interpreter ships, listed in its own image | a minute |
| `graph` | the dependency graph with the CPAN Testers counts, for each interpreter (with `-perl 5.44.0`: without it the counts are not attached, and every distribution of CPAN is selected) | a minute |
| `run` | perl, then gperl, on the reference set and what it needs | about 2 hours for perl, 4 for gperl |
| `report` | the failure records and the comparison, in each run's directory | minutes |
| `export` | the snapshot, the reference set and both runs into `results/` | minutes |
| `site` | the site, into `_site/` | seconds |

The durations of `run` are those of the provisional runs, with the limits of the
Makefile (`JOBS=16 CPUS=1 MEMORY=1500m TEST_LIMIT=300s`). These, like every variable
of the Makefile (`DATA`, `REF_DATA`, `RUN`, `REF_RUN`, `PERL_VERSION`, `RESULTS`, ...),
can be set on the command line. The runs are named `gperl-<commit>` and
`perl-5.44.0`; their records and logs are in `data/gperl/runs/<run>/` and
`data/perl/runs/<run>/`, and `export` writes them into `results/<run>/`. To export
the runs as provisional, add `PROVISIONAL=1 NOTE='what they are'`.

`results/` holds the runs of one CPAN snapshot: before exporting the runs of a new
snapshot, remove those of the old one (`git rm -r results/<run>`).

### Publishing

Commit `results/` on a branch and open a pull request. Once it is merged into `main`,
the workflow `.github/workflows/pages.yml` tests the tool, generates the site and
deploys it to GitHub Pages; it can also be started by hand from the Actions tab.
GitHub Pages has to be enabled once for the repository, with GitHub Actions as its
source (Settings, Pages, Build and deployment, Source: GitHub Actions).

## License

MIT; see `LICENSE`.
