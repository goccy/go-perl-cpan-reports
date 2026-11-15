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

The results in `results/` are **provisional**: they were measured with gperl built from
go-perl commit `84f3ee6`, on an unreleased development branch of go-perl. The results
published in the end will be measured again with the released gperl.

| run | interpreter | passes, of 14,318 |
|---|---|---|
| `full7` | gperl, go-perl `84f3ee6` | 12,972 (90.6%) |
| `ref2` | perl 5.44.0 | 13,981 (97.6%) |

go-perl passes 92.6% of what perl passes. Both runs: the CPAN snapshot of 2026-11-10,
16 distributions at a time, each container with one CPU and 1500 MiB of memory, the
test phase of a distribution limited to five minutes, on linux/arm64.

## Repository layout

```
cmd/cpancompat/          the survey tool (one command, cpancompat)
cmd/cpancompat/image/    the files of the container images distributions are tested in
cmd/cpancompat/site/     the templates and assets of the site
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
| `report` | why each distribution that did not pass failed, and how a run compares with a run of perl |
| `refset`, `export` | write the snapshot, the reference set and a run into `results/` |
| `site` | generates the site from `results/` |

`cpancompat` without arguments lists every option.

### The results

```
results/snapshot.json           the CPAN snapshot and the size of the reference set
results/reference.jsonl.gz      the reference set: distribution, release, CPAN Testers
                                Linux pass and fail counts on perl 5.44.0, dependents
results/<run>/meta.json         the interpreter and its commit, whether the run is
                                provisional, the image, the dates, and the limits and
                                harness of each invocation of `run`; the network policy
results/<run>/results.jsonl.gz  the result of each distribution: outcome, phases, test files
results/<run>/failures.jsonl.gz why each distribution that did not pass failed: failing
                                phase and test files, evidence lines, cause, blockers
results/<run>/logs.jsonl.gz     the end of the failing phase's log of each of them
```

The files are gzip-compressed JSON lines. The records hold no path of the machine a
run was made on: `export` replaces the data directory's path and cuts paths below the
home directory short.

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
    - location: /path/to/go-perl-cpan-testers/data/gperl/archives
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
  unreleased go-perl development branches (commit `84f3ee6`, with an interpreter build
  that is not published), which cannot be rebuilt from public sources. A survey of the
  released gperl, or of any public go-perl commit, can be.

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
