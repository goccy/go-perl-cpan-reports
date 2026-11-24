# Reproduces a survey: gperl and perl itself on the reference set, then the
# results the repository keeps and the site. README.md explains the
# prerequisites (Docker in a virtual machine of its own, a gperl binary for
# Linux) and the steps; `make survey GPERL=... GPERL_COMMIT=...` runs them
# all. Every step can be run again on its own; an interrupted run continues
# where it stopped.

# The gperl binary, built for Linux on the architecture of the containers,
# and the go-perl commit it was built from.
GPERL        ?=
GPERL_COMMIT ?=
GPERL_SHORT  := $(shell printf '%.7s' '$(GPERL_COMMIT)')

# The perl version gperl's interpreter is built from: its CPAN Testers
# reports define the reference set, and its official image is the
# reference the interpreter is compared with.
PERL_VERSION ?= 5.44.0
PERL_IMAGE   ?= perl:$(PERL_VERSION)-slim-bookworm
# The image the survey image around gperl is built on (the FROM line of
# cmd/cpancompat/image/Dockerfile).
GPERL_BASE_IMAGE := debian:bookworm-slim

# The data directories (downloads, graphs, archives, the records and logs
# of the runs): one for gperl, one for perl, which shares the downloads
# and the archives of the first.
DATA     ?= $(CURDIR)/data/gperl
REF_DATA ?= $(CURDIR)/data/perl

RUN       ?= gperl-$(GPERL_SHORT)
REF_RUN   ?= perl-$(PERL_VERSION)
IMAGE     ?= cpancompat:$(RUN)
REF_IMAGE ?= cpancompat-ref:$(PERL_VERSION)

# The limits the published runs use: 16 distributions at a time, each
# container with one CPU and 1500 MiB, the test phase of a distribution
# limited to five minutes.
JOBS       ?= 16
CPUS       ?= 1
MEMORY     ?= 1500m
TEST_LIMIT ?= 300s

# The static build phase: its image, how many build containers run at once,
# the CPUs and memory of each, and how long one of their phases may run.
# DEV_REPLACE is the go-perl version whose source the builds use, for a
# gperl whose `gperl build` cannot resolve go-perl itself.
STATIC_IMAGE  ?= cpancompat-static:$(RUN)
STATIC_JOBS   ?= 4
STATIC_CPUS   ?= 2
STATIC_MEMORY ?= 4g
STATIC_WALL   ?= 1h
DEV_REPLACE   ?=

# Where the results the repository keeps and the site go. With
# PROVISIONAL=1, the runs are exported as provisional, with NOTE saying
# what they are.
RESULTS     ?= results
SITE        ?= _site
SERVE       ?= localhost:8000
PROVISIONAL ?=
NOTE        ?=

CPANCOMPAT := $(CURDIR)/bin/cpancompat
SHARED     := 02packages.details.txt.gz releases.jsonl releases-indexed-only.jsonl snapshot.json testers-perl-$(PERL_VERSION).jsonl
EXPORT_FLAGS = $(if $(PROVISIONAL),-provisional -note '$(NOTE)')

.PHONY: survey snapshot images provided graph run run-gperl run-perl static report export site serve test \
	require-gperl require-commit $(CPANCOMPAT)

survey: snapshot images provided graph run static report export site

# go build in the checkout records the revision, which run keeps with
# the settings of each invocation.
$(CPANCOMPAT):
	go build -o $@ ./cmd/cpancompat

require-gperl:
	@test -n '$(GPERL)' -a -f '$(GPERL)' || { echo 'GPERL=path/to/gperl (built for linux) is needed' >&2; exit 2; }

require-commit:
	@test -n '$(GPERL_COMMIT)' || { echo 'GPERL_COMMIT=<the go-perl commit gperl was built from> is needed' >&2; exit 2; }

# The CPAN snapshot and the CPAN Testers reports on PERL_VERSION, shared by
# the perl data directory.
snapshot: $(CPANCOMPAT)
	$(CPANCOMPAT) fetch -data $(DATA)
	$(CPANCOMPAT) testers -data $(DATA) -perl $(PERL_VERSION)
	mkdir -p $(DATA)/archives $(REF_DATA)
	for f in $(SHARED) archives; do ln -sfn $(DATA)/$$f $(REF_DATA)/$$f; done

images: $(CPANCOMPAT) require-gperl require-commit
	$(CPANCOMPAT) image -data $(DATA) -gperl $(GPERL) -image $(IMAGE)
	$(CPANCOMPAT) image -data $(REF_DATA) -perl-image $(PERL_IMAGE) -image $(REF_IMAGE)
	$(CPANCOMPAT) image -data $(DATA) -static -gperl $(GPERL) $(if $(DEV_REPLACE),-dev-replace $(DEV_REPLACE)) -image $(STATIC_IMAGE)

# The modules each interpreter ships, listed by provided.pl run in its own
# survey image (no network, no capabilities); the graph needs them.
provided: require-commit
	docker run -i --rm --network none --cap-drop ALL --security-opt no-new-privileges $(IMAGE) \
		sh -c 'cat > /tmp/provided.pl && perl /tmp/provided.pl' < cmd/cpancompat/provided.pl > $(DATA)/provided.json.tmp
	mv $(DATA)/provided.json.tmp $(DATA)/provided.json
	docker run -i --rm --network none --cap-drop ALL --security-opt no-new-privileges $(REF_IMAGE) \
		sh -c 'cat > /tmp/provided.pl && perl /tmp/provided.pl' < cmd/cpancompat/provided.pl > $(REF_DATA)/provided.json.tmp
	mv $(REF_DATA)/provided.json.tmp $(REF_DATA)/provided.json

# Without -perl the CPAN Testers reports are not attached to the graph,
# and run would select every distribution instead of the reference set.
graph: $(CPANCOMPAT)
	$(CPANCOMPAT) graph -data $(DATA) -perl $(PERL_VERSION) > /dev/null
	$(CPANCOMPAT) graph -data $(REF_DATA) -perl $(PERL_VERSION) > /dev/null

run: run-perl run-gperl

run-perl: $(CPANCOMPAT)
	$(CPANCOMPAT) run -data $(REF_DATA) -run $(REF_RUN) -image $(REF_IMAGE) -interpreter perl@$(PERL_VERSION) \
		-jobs $(JOBS) -cpus $(CPUS) -memory $(MEMORY) -test-limit $(TEST_LIMIT)

run-gperl: $(CPANCOMPAT) require-commit
	$(CPANCOMPAT) run -data $(DATA) -run $(RUN) -image $(IMAGE) -interpreter go-perl@$(GPERL_SHORT) \
		-jobs $(JOBS) -cpus $(CPUS) -memory $(MEMORY) -test-limit $(TEST_LIMIT)

# The static builds of what passed on gperl.
static: $(CPANCOMPAT) require-commit
	$(CPANCOMPAT) static -data $(DATA) -run $(RUN) -image $(STATIC_IMAGE) \
		-jobs $(STATIC_JOBS) -cpus $(STATIC_CPUS) -memory $(STATIC_MEMORY) -wall $(STATIC_WALL)

# Text reports, failures.jsonl and failures.md in each run's directory. The
# interpreter names decide where the known causes (known-causes.json) apply:
# to the go-perl run only.
report: $(CPANCOMPAT) require-commit
	$(CPANCOMPAT) report -data $(REF_DATA) -run $(REF_RUN) -interpreter-name perl
	$(CPANCOMPAT) report -data $(DATA) -run $(RUN) -interpreter-name go-perl \
		-reference-data $(REF_DATA) -reference-run $(REF_RUN)

export: $(CPANCOMPAT) require-commit
	$(CPANCOMPAT) refset -data $(REF_DATA) -results $(RESULTS)
	$(CPANCOMPAT) export -data $(REF_DATA) -run $(REF_RUN) -results $(RESULTS) \
		-interpreter-name perl -interpreter-version $(PERL_VERSION) -base-image $(PERL_IMAGE) $(EXPORT_FLAGS)
	$(CPANCOMPAT) export -data $(DATA) -run $(RUN) -results $(RESULTS) \
		-interpreter-name go-perl -interpreter-commit $(GPERL_COMMIT) -base-image $(GPERL_BASE_IMAGE) \
		-compared-with $(REF_RUN) $(EXPORT_FLAGS)

site: $(CPANCOMPAT)
	$(CPANCOMPAT) site -results $(RESULTS) -out $(SITE)

serve: $(CPANCOMPAT)
	$(CPANCOMPAT) site -results $(RESULTS) -out $(SITE) -serve $(SERVE)

test:
	go vet ./...
	go test ./...
