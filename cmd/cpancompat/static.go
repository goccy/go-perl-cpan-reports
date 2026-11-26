package main

// The static build phase (cpancompat static): whether gperl builds a
// program that uses a distribution into one binary that runs where there is
// nothing but the binary.
//
// The distributions measured (the targets) are the ones whose tests passed
// in a run. For each, cpancompat writes a program that loads the
// distribution's modules and a cpanfile.snapshot of the distribution and
// everything it needs, and has gperl build the program with cgo, linked
// fully statically (static.sh, in the image of `cpancompat image -static`).
// The binary is then run in a container whose only file is the binary,
// with a writable /tmp (the binary unpacks its program and the standard
// library there), no network and an unprivileged user.
//
// Every distribution is built once, in gperl's module directory, a volume
// all containers share. A distribution's container starts only once every
// distribution its program needs has been built (or has failed to) in a
// container of its own, so that no two containers build the same
// distribution. What each distribution needs is what its MYMETA.json says,
// the requirements its configure step settled on, which gperl keeps in the
// module directory once it has built it; the static metadata stands in for
// it until then, and for a distribution that is installed without a
// configure step. A target whose program turns out to need more once the
// distributions it needs were built is linked again with them.
//
// Distributions a target needs that did not pass themselves are built as
// dependencies only (state "dependency"), and are not measured.
//
// A static target's own tests then run with its test binary (gperl test
// -c of its tree, with a snapshot that adds what its tests need, which it
// waits for too): the test files the run's harness reported on, in the
// empty container, and when they fail there, in the build image and with
// gperl test, which tells why (staticTest).

import (
	"archive/tar"
	"bytes"
	"cmp"
	"container/heap"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The states of a distribution in the static build phase.
const (
	// staticStatic: the program built into a fully static binary, which
	// loaded the distribution's modules in the empty container.
	staticStatic = "static"
	// staticNotStatic: it did not; the reason says where it stopped.
	staticNotStatic = "not_static"
	// staticBlocked: a distribution the program needs could not be built,
	// so neither could the program (BlockedBy names it).
	staticBlocked = "blocked"
	// staticDependency: the distribution did not pass in the run and was
	// only built because targets need it; not measured.
	staticDependency = "dependency"
	// staticDriverFailed: the harness could not record a result.
	staticDriverFailed = "driver_failed"
)

// The reasons of a state.
const (
	// reasonBundlesSystemLibraries (static): the binary carries libraries
	// of the system, linked from their static archives (ExternalLibs).
	reasonBundlesSystemLibraries = "bundles_system_libraries"
	// reasonArchiveNotSupported: gperl takes no archive whose name is not
	// Name-Version.tar.gz (.tgz, .tar.bz2, .zip).
	reasonArchiveNotSupported = "archive_not_supported"
	// reasonDistBuildFailed: the distribution's own build failed in the
	// module directory (gperl kept its log).
	reasonDistBuildFailed = "dist_build_failed"
	// reasonLinkerOverridden: the distribution built, and gperl refused
	// what it built: a native module its own linker made, which cannot be
	// linked statically.
	reasonLinkerOverridden = "linker_overridden"
	// reasonModulesTimeout: building the distributions of the closure ran
	// out of time.
	reasonModulesTimeout = "modules_timeout"
	// reasonLinkTimeout: the link of the program ran out of time.
	reasonLinkTimeout = "link_timeout"
	// reasonSharedLibraryPathDropped: a native module names a shared
	// library by its path, which gperl does not pass to the static link.
	reasonSharedLibraryPathDropped = "shared_library_path_dropped"
	// reasonNeedsSharedLibrary: a native module links a library of which
	// there is no static archive.
	reasonNeedsSharedLibrary = "needs_shared_library"
	// reasonCrossModuleSymbols: a native module uses symbols another one
	// defines, which a static link of gperl's modules does not resolve.
	reasonCrossModuleSymbols = "cross_module_symbols"
	// reasonCRuntimeNotLinked: a native module uses functions of a part of
	// the C library (libm, libcrypt) it did not name for its link.
	reasonCRuntimeNotLinked = "c_runtime_not_linked"
	// reasonLinkFailed: the link failed otherwise.
	reasonLinkFailed = "link_failed"
	// reasonNotFullyStatic: the binary names a program interpreter or
	// shared libraries.
	reasonNotFullyStatic = "not_fully_static"
	// reasonNeedsEnvironment: the binary failed in the empty container and
	// ran in the build environment.
	reasonNeedsEnvironment = "needs_environment"
	// reasonLoadFailed: the binary failed in both.
	reasonLoadFailed = "load_failed"
	// reasonLoadTimeout: the binary ran out of time in the empty container.
	reasonLoadTimeout = "load_timeout"
)

// The states of a static target's own tests (staticTest), run by its test
// binary (gperl test -c) once its program is static.
const (
	// testPass: the test files passed in the empty container.
	testPass = "pass"
	// testNeedsEnvironment: they failed there and passed with the same
	// binary in the build environment.
	testNeedsEnvironment = "needs_environment"
	// testStaticFailed: they failed with the binary in both, and passed
	// on the same project with gperl test.
	testStaticFailed = "static_failed"
	// testDiffersFromMakeTest: they failed with gperl test too; they
	// passed under make test in the run.
	testDiffersFromMakeTest = "differs_from_make_test"
	// testTimeout: the binary ran out of time in the empty container
	// (Stuck says whether it was blocked, spinning or slow).
	testTimeout = "timeout"
	// testDepsFailed: a distribution only the tests need was not built.
	testDepsFailed = "deps_failed"
	// testLinkFailed: gperl test -c did not build the test binary.
	testLinkFailed = "link_failed"
	// testNoRecord: the run recorded no test files of the distribution
	// (its test files are what the run's harness reported on).
	testNoRecord = "no_test_record"
)

// testEnvironment is the environment make test gives test files
// (ExtUtils::MakeMaker's test_via_harness, Test::Harness's runtests) and
// the run's dist.sh exports, which every test run (the test binary's,
// gperl test's) gets. dist.sh names itself, the test files' ancestor, as
// the CPAN client that runs ($$); in a test container that ancestor is its
// init, process 1.
var testEnvironment = []string{"PERL_DL_NONLAZY=1", "PERL_USE_UNSAFE_INC=1", "PERL_MM_USE_DEFAULT=1", "NONINTERACTIVE_TESTING=1",
	"PERL5_CPAN_IS_RUNNING=1", "PERL5_CPANPLUS_IS_RUNNING=1"}

// testBinaryPath is where a build container's test binary is, in its binary
// volume as the containers that run it mount that.
const testBinaryPath = "/app/app.test"

// testPids is the process limit of a test run: the run's test containers'
// (run.go's dist.sh container).
const testPids = "2048"

// The conditions of the empty container a binary is run in.
const (
	// emptyImage is the image without a single file (cpancompat image
	// -static makes it).
	emptyImage = "cpancompat-empty:latest"
	// loadLimit is how long a binary may run there.
	loadLimit = 120 * time.Second
	// loadMemory is the memory of that container, the survey's.
	loadMemory = "1500m"
	// loadTmpfs is the writable /tmp it gets, where programs may run as
	// they may in a /tmp on disk (a docker tmpfs is noexec unless told).
	loadTmpfs = "/tmp:rw,exec,size=1g,mode=1777"
	// loadUser is the user the binary runs as there.
	loadUser = "65534:65534"
)

// staticEnvironment describes the empty container in words, for meta.json.
const staticEnvironment = "The binary runs in a container whose image holds no file at all: no C library, no perl, " +
	"no /etc/passwd, no time zones, no certificates. It gets a writable tmpfs /tmp of 1 GiB (the binary " +
	"unpacks its program and the standard library there; without a writable temporary directory no gperl " +
	"binary starts), no network, user 65534 without capabilities, a read-only root file system, 1500 MiB " +
	"of memory, and 120 seconds."

// staticBuildFlags describes how the binaries are built, for meta.json.
const staticBuildFlags = "CGO_ENABLED=1 CC=gcc GOFLAGS=-ldflags=-extldflags=-static (glibc, fully static)"

// staticNative is one native module a program carries.
type staticNative struct {
	Package string `json:"package"`
	Dist    string `json:"dist"`
}

// externalLib is a library a native module of the program links that is
// not part of the C runtime (cRuntimeLibraries).
type externalLib struct {
	Name string `json:"name"`
	// Dist is the distribution whose native module links it.
	Dist string `json:"dist"`
	// From is where its static archive is: "system" (the compiler's
	// library directories, or another directory of the system the module
	// names) or "module_directory" (a distribution's tree, an Alien
	// library); "" when there is none.
	From          string `json:"from,omitempty"`
	StaticArchive bool   `json:"static_archive"`
}

// binaryInfo is what makes a binary depend on its environment.
type binaryInfo struct {
	Bytes int64 `json:"bytes"`
	// Interp is its program interpreter (PT_INTERP), "" for none.
	Interp string `json:"interp"`
	// Needed are the shared libraries it names (DT_NEEDED).
	Needed []string `json:"needed"`
}

// staticRecord is one line of runs/<run>/static.jsonl.
type staticRecord struct {
	Dist    string `json:"dist"`
	Release string `json:"release"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	// Loaded are the modules the program loads.
	Loaded []string `json:"loaded,omitempty"`
	// Closure is the distributions the program carries.
	Closure       []string       `json:"closure,omitempty"`
	NativeModules []staticNative `json:"native_modules,omitempty"`
	ExternalLibs  []externalLib  `json:"external_libs,omitempty"`
	// CausedBy names the other distributions whose native modules the
	// reason is found in.
	CausedBy  []string `json:"caused_by,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"`
	// ForcedWith is the closure of the container that made the record
	// when that one was forced to build distributions it waited for (see
	// force): a failure it recorded may be theirs.
	ForcedWith []string      `json:"forced_with,omitempty"`
	Binary     *binaryInfo   `json:"binary,omitempty"`
	Phases     []phaseRecord `json:"phases,omitempty"`
	Attempts   int           `json:"attempts"`
	Seconds    int           `json:"seconds"`
	Finished   string        `json:"finished"`
	// Test is how the target's own tests went with its test binary; nil
	// when they did not run (the program is not static, or the record is
	// not a target's).
	Test *staticTest `json:"test,omitempty"`
}

// staticTest is how a static target's own test files went with its test
// binary. Their phases are in the record's Phases.
type staticTest struct {
	State string `json:"state"`
	// Files counts the test files the binary was given.
	Files int `json:"files,omitempty"`
	// Stuck is what the processes of a binary that ran out of time were
	// doing: blocked, spinning or slow (see stuckOf).
	Stuck string `json:"stuck,omitempty"`
	// BlockedBy names the distributions only the tests need that were not
	// built (deps_failed).
	BlockedBy []string `json:"blocked_by,omitempty"`
}

// moduleBuildFailed reports whether the record says the distribution was
// not built in the module directory, so that no program that needs it can
// be built either. (A distribution whose record the harness could not make
// is not known to be built: a program that needed it would build it in a
// container of its own, beside other such programs.)
func (r *staticRecord) moduleBuildFailed() bool {
	switch r.State {
	case staticDriverFailed, staticBlocked:
		return true
	case staticNotStatic, staticDependency:
		switch r.Reason {
		case reasonArchiveNotSupported, reasonDistBuildFailed, reasonLinkerOverridden, reasonModulesTimeout:
			return true
		}
	}
	return false
}

type staticOptions struct {
	dataDir string
	run     string
	image   string
	top     int
	only    []string
	redo    map[string]bool
	jobs    int
	cpus    string
	memory  string
	// wall is how long one phase of a build container may run.
	wall time.Duration
}

// staticInvocation is one line of runs/<run>/static-invocations.jsonl.
type staticInvocation struct {
	Started     string         `json:"started"`
	Image       string         `json:"image"`
	Platform    string         `json:"platform"`
	Harness     harnessVersion `json:"harness"`
	Jobs        int            `json:"jobs"`
	CPUs        string         `json:"cpus"`
	Memory      string         `json:"memory"`
	WallSeconds int            `json:"wall_seconds"`
	Top         int            `json:"top,omitempty"`
	Dists       []string       `json:"dists,omitempty"`
	Redo        []string       `json:"redo,omitempty"`
}

// staticNode is one distribution in the schedule. Only the coordinating
// goroutine reads or writes it.
type staticNode struct {
	d *dist
	// target: measured; otherwise built as a dependency only.
	target bool
	// waiting holds the distributions that must be settled before this
	// one starts (again).
	waiting map[string]bool
	// forced: started while distributions of its closure were not settled,
	// to end a standstill (they need one another); its container builds
	// them.
	forced   bool
	attempts int
	started  time.Time
	rec      *staticRecord
}

// staticReadyHeap orders the distributions that can start now: the one
// most others depend on first.
type staticReadyHeap []*staticNode

func (h staticReadyHeap) Len() int { return len(h) }
func (h staticReadyHeap) Less(i, j int) bool {
	if h[i].d.DependentsTotal != h[j].d.DependentsTotal {
		return h[i].d.DependentsTotal > h[j].d.DependentsTotal
	}
	return h[i].d.Dist < h[j].d.Dist
}
func (h staticReadyHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *staticReadyHeap) Push(x any)   { *h = append(*h, x.(*staticNode)) }
func (h *staticReadyHeap) Pop() any {
	old := *h
	n := old[len(old)-1]
	*h = old[:len(old)-1]
	return n
}

// distRequirements is what one distribution needs: the requirement lines of
// its snapshot entry and the distributions providing them.
type distRequirements struct {
	lines []snapshotPackage
	needs []string
}

// snapshotPackage is one "Name version" line of a carton snapshot.
type snapshotPackage struct {
	name, version string
}

type staticRunner struct {
	opts     staticOptions
	dists    map[string]*dist
	byModule map[string]string
	versions map[string]string
	provided map[string]string
	targets  map[string]bool
	previous map[string]*staticRecord

	nodes   map[string]*staticNode
	waiters map[string][]*staticNode
	ready   staticReadyHeap
	total   int
	done    int

	runDir    string
	staticDir string
	treesDir  string
	archives  string
	// modVolume is gperl's module directory, shared by all containers.
	modVolume string
	// cacheImage is the image with the Go build and module caches of a
	// program without dependencies (makeCacheImage); every build container
	// starts from it, and what a container adds to them goes with it.
	cacheImage string
	label      string
	results   *os.File

	probe *staticProbe
	// requirements caches what each distribution needs, as far as is
	// known (requirementsOf).
	requirements map[string]*distRequirements
	// verified are the distributions whose archives are in the data
	// directory, checked.
	verified map[string]bool
	// testRequirements caches what each target needs for its tests too
	// (requirementsOf with testPhases).
	testRequirements map[string]*distRequirements
	// testFiles are, by distribution, the test files the run's harness
	// reported on: the ones its test binary runs.
	testFiles map[string][]string
	// testLimit and testMemory are what the run gave a distribution's
	// test phase: how long it may run, and the memory of its container.
	testLimit  time.Duration
	testMemory string
}

// staticAttempted is what a worker hands back: the record, and the
// distributions whose trees its container collected.
type staticAttempted struct {
	job       *staticJob
	rec       *staticRecord
	collected []string
}

// runStatic runs the static build phase of a run.
func runStatic(opts staticOptions) (err error) {
	s := &staticRunner{
		opts: opts, dists: map[string]*dist{}, byModule: map[string]string{},
		targets: map[string]bool{}, previous: map[string]*staticRecord{},
		nodes: map[string]*staticNode{}, waiters: map[string][]*staticNode{},
		runDir:       filepath.Join(opts.dataDir, "runs", opts.run),
		modVolume:    "cpancompat-" + opts.run + "-static-mod",
		cacheImage:   "cpancompat-static-cache:" + opts.run,
		label:        "cpancompat-static=" + opts.run,
		requirements: map[string]*distRequirements{}, verified: map[string]bool{},
		testRequirements: map[string]*distRequirements{}, testFiles: map[string][]string{},
	}
	s.staticDir = filepath.Join(s.runDir, "static")
	s.treesDir = filepath.Join(s.staticDir, "trees")
	if s.archives, err = archivesDir(opts.dataDir); err != nil {
		return err
	}
	all, err := readDists(filepath.Join(opts.dataDir, distsFile))
	if err != nil {
		return err
	}
	for i := range all {
		d := &all[i]
		s.dists[d.Dist] = d
		for _, m := range d.Modules {
			s.byModule[m] = d.Dist
		}
	}
	index, err := readPackageIndex(filepath.Join(opts.dataDir, packagesFile))
	if err != nil {
		return err
	}
	s.versions = make(map[string]string, len(index))
	for module, e := range index {
		s.versions[module] = e.version
	}
	if s.provided, err = readProvided(filepath.Join(opts.dataDir, providedFile)); err != nil {
		return err
	}
	rows, err := readJSONLines[result](filepath.Join(s.runDir, "results.jsonl"))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("static: run %s has no results", opts.run)
	}
	var passed []*dist
	for _, res := range latestResults(rows) {
		if res.Outcome != outcomePass {
			continue
		}
		for _, f := range res.Tests {
			s.testFiles[res.Dist] = append(s.testFiles[res.Dist], f.File)
		}
		d := s.dists[res.Dist]
		if d == nil {
			return fmt.Errorf("static: run %s recorded %s, which %s does not have", opts.run, res.Dist, distsFile)
		}
		passed = append(passed, d)
	}
	targets, err := staticTargets(passed, opts.only, opts.top)
	if err != nil {
		return err
	}
	if s.testLimit, s.testMemory, err = runTestSettings(filepath.Join(s.runDir, "invocations.jsonl")); err != nil {
		return err
	}
	for _, name := range targets {
		s.targets[name] = true
	}

	for _, dir := range []string{s.treesDir, filepath.Join(s.staticDir, "dists")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	recordsPath := filepath.Join(s.runDir, "static.jsonl")
	earlier, err := readJSONLines[staticRecord](recordsPath)
	if err != nil {
		return err
	}
	for i := range earlier {
		rec := &earlier[i]
		if opts.redo[rec.State] || opts.redo[rec.Reason] {
			delete(s.previous, rec.Dist)
		} else {
			s.previous[rec.Dist] = rec
		}
	}
	if s.results, err = os.OpenFile(recordsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.results.Close()) }()

	if err := s.prepareDocker(); err != nil {
		return err
	}
	if s.probe, err = s.runProbe(len(earlier) > 0); err != nil {
		return err
	}
	if err := s.makeCacheImage(); err != nil {
		return err
	}
	if err := s.warm(); err != nil {
		return err
	}
	if err := recordStaticInvocation(s.runDir, opts); err != nil {
		return err
	}
	for _, name := range targets {
		s.ensureNode(name)
	}
	fmt.Fprintf(os.Stderr, "cpancompat: %d distributions selected, %d scheduled, %d already recorded\n",
		len(targets), s.total, s.done)
	return s.schedule(s.attempt)
}

// staticTargets returns the names of the distributions to measure, of the
// ones that passed: those named with -dists, or else the -top most
// depended-upon, or all.
func staticTargets(passed []*dist, only []string, top int) ([]string, error) {
	if len(only) > 0 {
		ok := map[string]bool{}
		for _, d := range passed {
			ok[d.Dist] = true
		}
		for _, name := range only {
			if !ok[name] {
				return nil, fmt.Errorf("static: %s did not pass in the run", name)
			}
		}
		return only, nil
	}
	order := slices.Clone(passed)
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].DependentsTotal != order[j].DependentsTotal {
			return order[i].DependentsTotal > order[j].DependentsTotal
		}
		return order[i].Dist < order[j].Dist
	})
	if top > 0 && top < len(order) {
		order = order[:top]
	}
	names := make([]string, len(order))
	for i, d := range order {
		names[i] = d.Dist
	}
	return names, nil
}

// runTestSettings returns the test phase's time limit and the containers'
// memory of the run's latest invocation (invocations.jsonl) that recorded
// a limit: what a test binary runs with.
func runTestSettings(name string) (time.Duration, string, error) {
	invs, err := readJSONLines[invocation](name)
	if err != nil {
		return 0, "", err
	}
	for i := len(invs) - 1; i >= 0; i-- {
		if invs[i].TestSeconds > 0 {
			if invs[i].Memory == "" {
				return 0, "", fmt.Errorf("static: %s: the invocation that records the test limit records no memory", name)
			}
			return time.Duration(invs[i].TestSeconds) * time.Second, invs[i].Memory, nil
		}
	}
	return 0, "", fmt.Errorf("static: %s records no test limit", name)
}

// recordStaticInvocation appends what this invocation runs with to the run's
// static-invocations.jsonl.
func recordStaticInvocation(runDir string, opts staticOptions) error {
	inv := staticInvocation{
		Started: time.Now().UTC().Format(time.RFC3339), Image: opts.image, Harness: thisHarness(),
		Jobs: opts.jobs, CPUs: opts.cpus, Memory: opts.memory, WallSeconds: int(opts.wall / time.Second),
		Top: opts.top, Dists: opts.only,
	}
	for v := range opts.redo {
		inv.Redo = append(inv.Redo, v)
	}
	sort.Strings(inv.Redo)
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}").Output()
	if err != nil {
		return fmt.Errorf("docker version: %w", err)
	}
	inv.Platform = strings.TrimSpace(string(out))
	return appendJSONLine(filepath.Join(runDir, "static-invocations.jsonl"), inv)
}

// prepareDocker removes what an interrupted invocation left (its containers
// and binary volumes carry the run's label), makes the shared volumes and
// fills them, and puts the network rules in place.
func (s *staticRunner) prepareDocker() error {
	out, err := exec.Command("docker", "ps", "--all", "--quiet", "--filter", "label="+s.label).Output()
	if err != nil {
		return fmt.Errorf("docker ps: %w", err)
	}
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		if out, err := exec.Command("docker", append([]string{"rm", "--force"}, ids...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("docker rm: %v: %s", err, out)
		}
	}
	out, err = exec.Command("docker", "volume", "ls", "--quiet", "--filter", "label="+s.label).Output()
	if err != nil {
		return fmt.Errorf("docker volume ls: %w", err)
	}
	if names := strings.Fields(string(out)); len(names) > 0 {
		if out, err := exec.Command("docker", append([]string{"volume", "rm"}, names...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("docker volume rm: %v: %s", err, out)
		}
	}
	// The records say which distributions the module directory has built:
	// without it, programs would build them again, side by side.
	out, err = exec.Command("docker", "volume", "ls", "--quiet", "--filter", "name=^"+s.modVolume+"$").Output()
	if err != nil {
		return fmt.Errorf("docker volume ls: %w", err)
	}
	if len(strings.Fields(string(out))) == 0 && len(s.previous) > 0 {
		return fmt.Errorf("the run's module directory (volume %s) is gone and its records are not: they cannot be continued", s.modVolume)
	}
	if out, err := exec.Command("docker", "volume", "create", s.modVolume).CombinedOutput(); err != nil {
		return fmt.Errorf("docker volume create: %v: %s", err, out)
	}
	// Docker fills an empty volume from the image the first time a
	// container mounts it (see runSurvey). The directories each container
	// mounts a tmpfs of its own over are made here, as the user: Docker
	// would make a missing one as root, and the module directory's cache
	// directory with it, where gperl then could not write.
	if out, err := exec.Command("docker", "run", "--rm", "--network", "none",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--ulimit", "core=0",
		"-v", s.modVolume+":/opt/gperl/mod",
		s.opts.image, "mkdir", "-p", "/opt/gperl/mod/cache/sdk", "/opt/gperl/mod/cache/linkset",
		"/opt/gperl/mod/cache/lib", "/opt/gperl/mod/cache/download").CombinedOutput(); err != nil {
		return fmt.Errorf("filling the volumes from %s: %v: %s", s.opts.image, err, out)
	}
	return guardNetwork(s.opts.dataDir)
}

// buildArgs are the docker create arguments of a build container, up to
// the image: the module directory, a binary volume of its own at /bin-out,
// and its own tmpfs where gperl writes per process (the SDK headers, which
// it writes again for every build) or per program (the link set and the
// merged module tree), so that containers neither change one another's
// files nor leave theirs behind. The Go caches are the image's
// (makeCacheImage): what a program adds to them, nothing else uses, and it
// goes with the container.
func (s *staticRunner) buildArgs(binVolume string, offlineGo bool) []string {
	args := []string{
		"--init", "--network", surveyNetwork,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--ulimit", "core=0",
		"--cpus", s.opts.cpus, "--memory", s.opts.memory, "--pids-limit", "4096",
		"-v", s.modVolume + ":/opt/gperl/mod",
		"-v", binVolume + ":/bin-out",
		"-v", s.archives + ":/archives:ro",
		"--tmpfs", "/opt/gperl/mod/cache/sdk:rw,mode=1777",
		"--tmpfs", "/opt/gperl/mod/cache/linkset:rw,mode=1777",
		"--tmpfs", "/opt/gperl/mod/cache/lib:rw,exec,mode=1777",
	}
	if offlineGo {
		// The Go modules every program needs are in the cache image's
		// module cache.
		args = append(args, "-e", "GOPROXY=off")
	}
	return args
}

// makeCacheImage makes the cache image: the image with the Go build and
// module caches a program without dependencies leaves (static.sh cache),
// committed from the container that built it. Kept in a volume, the caches
// would grow by every program's own packages (its embedded module tree, its
// native modules), which no other program uses.
func (s *staticRunner) makeCacheImage() error {
	name := "cpancompat-" + s.opts.run + "-static-cache"
	log, err := os.Create(filepath.Join(s.staticDir, "cache.log"))
	if err != nil {
		return err
	}
	end, _, err := runContainer(name, s.label, []string{"--network", surveyNetwork, "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--ulimit", "core=0", "--cpus", s.opts.cpus, "--memory", s.opts.memory,
		s.opts.image, "bash", "/opt/cpancompat/static.sh", "cache"}, nil, log, 0)
	if err = errors.Join(err, log.Close()); err == nil && end.ExitCode != 0 {
		err = fmt.Errorf("static.sh cache ended with status %d%s (%s)", end.ExitCode, end.errorText(), log.Name())
	}
	var old []byte
	if err == nil {
		// The image an earlier invocation made goes once this one has its
		// own: no container uses it any more (prepareDocker).
		old, err = exec.Command("docker", "image", "ls", "--quiet", s.cacheImage).Output()
	}
	if err == nil {
		if out, cerr := exec.Command("docker", "commit", name, s.cacheImage).CombinedOutput(); cerr != nil {
			err = fmt.Errorf("docker commit: %v: %s", cerr, out)
		}
	}
	if ids := strings.Fields(string(old)); err == nil && len(ids) > 0 {
		if out, rerr := exec.Command("docker", append([]string{"image", "rm"}, ids...)...).CombinedOutput(); rerr != nil {
			err = fmt.Errorf("docker image rm: %v: %s", rerr, out)
		}
	}
	if rmErr := removeContainers(name); rmErr != nil {
		err = errors.Join(err, rmErr)
	}
	if err != nil {
		return fmt.Errorf("cache image: %w", err)
	}
	return nil
}

// staticProbe is what the image says about its toolchain.
type staticProbe struct {
	// searchDirs are the compiler's library directories, in its order.
	searchDirs []string
	// archives maps the name of a static archive in them to its path in
	// the first directory that has it.
	archives map[string]string
	runtime  runtimeSymbols
	gperl    gperlBuild
}

// runProbe asks the image for its compiler's library directories, the
// static archives in them and the C runtime's shared libraries
// (static.sh probe), into runs/<run>/static/probe.
//
// The records, the collected trees and the module directory of a run are
// only ever continued with the toolchain they were made with: a probe that
// names another gperl, compiler, C library or Go than the run's earlier one
// is an error.
func (s *staticRunner) runProbe(recorded bool) (*staticProbe, error) {
	dir := filepath.Join(s.staticDir, "probe")
	fresh := dir + ".new"
	if err := os.RemoveAll(fresh); err != nil {
		return nil, err
	}
	name := "cpancompat-" + s.opts.run + "-static-probe"
	end, _, err := runContainer(name, s.label,
		[]string{"--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--ulimit", "core=0",
			s.opts.image, "bash", "/opt/cpancompat/static.sh", "probe"}, nil, os.Stderr, 0)
	if err == nil && end.ExitCode != 0 {
		err = fmt.Errorf("static.sh probe ended with status %d%s", end.ExitCode, end.errorText())
	}
	if err == nil {
		err = copyOut(name, fresh)
	}
	if rmErr := removeContainers(name); rmErr != nil {
		err = errors.Join(err, rmErr)
	}
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	for _, f := range []string{"gperl.json", "versions.json"} {
		if !recorded {
			break
		}
		old, err := os.ReadFile(filepath.Join(dir, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		now, err := os.ReadFile(filepath.Join(fresh, f))
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(old, now) {
			return nil, fmt.Errorf("probe: %s says %s, the run's earlier invocations had %s: remove its static build phase (static.jsonl, static-invocations.jsonl, static/ and its volumes) to make it again with another toolchain",
				f, bytes.TrimSpace(now), bytes.TrimSpace(old))
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.Rename(fresh, dir); err != nil {
		return nil, err
	}
	return readProbe(dir)
}

// readProbe reads the probe's records.
func readProbe(dir string) (*staticProbe, error) {
	p := &staticProbe{}
	raw, err := os.ReadFile(filepath.Join(dir, "search-dirs.txt"))
	if err != nil {
		return nil, err
	}
	p.searchDirs = strings.Fields(string(raw))
	if len(p.searchDirs) == 0 {
		return nil, fmt.Errorf("%s: no library directories", filepath.Join(dir, "search-dirs.txt"))
	}
	raw, err = os.ReadFile(filepath.Join(dir, "archives.txt"))
	if err != nil {
		return nil, err
	}
	p.archives = firstArchives(p.searchDirs, strings.Fields(string(raw)))
	if p.runtime, err = readRuntimeSymbols(filepath.Join(dir, "runtime")); err != nil {
		return nil, err
	}
	if raw, err = os.ReadFile(filepath.Join(dir, "gperl.json")); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p.gperl); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "gperl.json"), err)
	}
	return p, nil
}

// firstArchives maps each archive's file name to its path in the first of
// dirs that has it, the one the linker takes.
func firstArchives(dirs, paths []string) map[string]string {
	byDir := map[string][]string{}
	for _, p := range paths {
		byDir[path.Dir(p)] = append(byDir[path.Dir(p)], p)
	}
	out := map[string]string{}
	for _, dir := range dirs {
		for _, p := range byDir[dir] {
			if _, ok := out[path.Base(p)]; !ok {
				out[path.Base(p)] = p
			}
		}
	}
	return out
}

// warm builds a program without dependencies, which compiles go-perl into
// the shared Go build cache and fetches the Go modules every program needs
// into the module cache, and runs it in the empty container: when that
// fails, every distribution would.
func (s *staticRunner) warm() error {
	dir := filepath.Join(s.staticDir, "warm")
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	name := "cpancompat-" + s.opts.run + "-static-warm"
	input, err := appArchive(nil, nil, nil, nil)
	if err != nil {
		return err
	}
	b, err := s.build(name, dir, "warm", "-", input, false)
	if err != nil {
		return fmt.Errorf("warm: %w", err)
	}
	defer b.remove()
	phases, err := readJSONLines[phaseRecord](filepath.Join(dir, "phases.jsonl"))
	if err != nil {
		return fmt.Errorf("warm: %w", err)
	}
	if len(phases) != 1 || phases[0].Status != 0 {
		return fmt.Errorf("warm: building a program without dependencies failed (%s)", filepath.Join(dir, "link.log"))
	}
	if err := s.linksOwnGoPerl(name, filepath.Join(dir, "app")); err != nil {
		return fmt.Errorf("warm: %w", err)
	}
	run, err := s.runBinary(name+"-run", b.volume, filepath.Join(dir, "run_empty.log"), true)
	if err != nil {
		return fmt.Errorf("warm: %w", err)
	}
	if run.Status != 0 {
		return fmt.Errorf("warm: a program without dependencies failed in the empty container (%s)", filepath.Join(dir, "run_empty.log"))
	}
	fmt.Fprintf(os.Stderr, "cpancompat: a program without dependencies builds in %ds and runs in the empty container\n", phases[0].Seconds)
	return nil
}

// linksOwnGoPerl copies the binary the build container name made to bin and
// checks, by its build information, that the program gperl generated
// links go-perl at gperl's own released version (not a replacement).
func (s *staticRunner) linksOwnGoPerl(name, bin string) error {
	if err := copyFileOut(name, "/bin-out/app", bin); err != nil {
		return err
	}
	info, err := buildinfo.ReadFile(bin)
	if err != nil {
		return err
	}
	want := s.probe.gperl.Main.Version
	var linked *debug.Module
	for _, dep := range info.Deps {
		if dep.Path == gperlModule {
			linked = dep
		}
	}
	if linked == nil || linked.Replace != nil || linked.Version != want {
		return fmt.Errorf("the program gperl %s built does not link %s %s as released: %+v", want, gperlModule, want, linked)
	}
	fmt.Fprintf(os.Stderr, "cpancompat: gperl %s builds programs that link %s %s (%s)\n", want, gperlModule, linked.Version, linked.Sum)
	return nil
}

// ensureNode schedules the distribution, settled when an earlier invocation
// recorded it. It returns nil for a name the index does not know.
func (s *staticRunner) ensureNode(name string) *staticNode {
	if n := s.nodes[name]; n != nil {
		return n
	}
	d := s.dists[name]
	if d == nil {
		return nil
	}
	n := &staticNode{d: d, target: s.targets[name], waiting: map[string]bool{}}
	s.nodes[name] = n
	s.total++
	// A target recorded as a dependency by an earlier, narrower invocation
	// is measured now.
	if prev := s.previous[name]; prev != nil && !(n.target && prev.State == staticDependency) {
		n.rec = prev
		s.done++
		return n
	}
	heap.Push(&s.ready, n)
	return n
}

// schedule runs the scheduled distributions, at most -jobs at once, each by
// attempt (on a goroutine of its own), until every one is settled.
func (s *staticRunner) schedule(attempt func(*staticJob) (*staticRecord, []string)) error {
	finished := make(chan staticAttempted)
	active, seq := 0, 0
	for {
		for active < s.opts.jobs && s.ready.Len() > 0 {
			n := heap.Pop(&s.ready).(*staticNode)
			closure, tests, err := s.closures(n)
			if err != nil {
				if err := s.recordUnreadable(n, err); err != nil {
					return err
				}
				continue
			}
			// A target waits for what its tests need too: no container but
			// that distribution's own builds it.
			wait := closure
			if tests != nil {
				wait = tests
			}
			if s.waitFor(n, wait) {
				continue
			}
			if rec := s.unbuildable(n, closure); rec != nil {
				if err := s.record(n, rec); err != nil {
					return err
				}
				continue
			}
			seq++
			if n.attempts == 0 {
				n.started = time.Now()
			}
			n.attempts++
			job, err := s.prepare(n, closure, tests, seq)
			if err != nil {
				fmt.Fprintf(os.Stderr, "cpancompat: %s: %v\n", n.d.Dist, err)
				rec := s.newRecord(n)
				rec.State, rec.Closure = staticDriverFailed, closure
				if err := s.record(n, rec); err != nil {
					return err
				}
				continue
			}
			active++
			go func() {
				rec, collected := attempt(job)
				finished <- staticAttempted{job, rec, collected}
			}()
		}
		if active == 0 {
			if s.force() {
				continue
			}
			break
		}
		a := <-finished
		active--
		if err := s.settle(a); err != nil {
			return err
		}
	}
	for name, n := range s.nodes {
		if n.rec == nil {
			return fmt.Errorf("static: %s was never settled", name)
		}
	}
	return nil
}

// waitFor makes n wait for the distributions of its closure that are not
// settled, scheduling the ones not yet scheduled, and reports whether there
// are any. A forced node waits for none.
func (s *staticRunner) waitFor(n *staticNode, closure []string) bool {
	if n.forced {
		return false
	}
	for _, name := range closure {
		m := s.ensureNode(name)
		if m == nil || m == n || m.rec != nil || n.waiting[name] {
			continue
		}
		n.waiting[name] = true
		s.waiters[name] = append(s.waiters[name], n)
	}
	return len(n.waiting) > 0
}

// unbuildable returns the final record of a distribution that cannot be
// built: gperl takes no archive of its name, or a distribution of its
// closure could not be built. It returns nil for one that can be tried.
func (s *staticRunner) unbuildable(n *staticNode, closure []string) *staticRecord {
	rec := s.newRecord(n)
	if _, err := gperlTreeName(n.d.Path); err != nil {
		rec.State, rec.Reason = staticNotStatic, reasonArchiveNotSupported
		if !n.target {
			rec.State = staticDependency
		}
		return rec
	}
	for _, name := range closure {
		m := s.nodes[name]
		if m == n || m == nil || m.rec == nil || !m.rec.moduleBuildFailed() {
			continue
		}
		// A member whose failure was recorded by a container forced to
		// build n too (see force) may have failed for n: n's own container
		// tells why n fails.
		if slices.Contains(m.rec.ForcedWith, n.d.Dist) {
			continue
		}
		rec.BlockedBy = append(rec.BlockedBy, name)
	}
	if len(rec.BlockedBy) == 0 {
		return nil
	}
	rec.State, rec.Closure = staticBlocked, closure
	if !n.target {
		// A dependency that cannot be built blocks what needs it the
		// same way.
		rec.State, rec.Reason = staticDependency, reasonDistBuildFailed
	}
	return rec
}

// force ends a standstill: nothing runs or can start, and distributions
// wait for one another (they need one another; their static metadata did
// not tell, or only their configure steps did). The first of them by name
// starts without waiting; its container builds the others of its closure,
// which then find themselves built. It reports whether it released one.
func (s *staticRunner) force() bool {
	var first *staticNode
	for _, n := range s.nodes {
		if n.rec == nil && len(n.waiting) > 0 && (first == nil || n.d.Dist < first.d.Dist) {
			first = n
		}
	}
	if first == nil {
		return false
	}
	for name := range first.waiting {
		kept := s.waiters[name][:0]
		for _, w := range s.waiters[name] {
			if w != first {
				kept = append(kept, w)
			}
		}
		s.waiters[name] = kept
	}
	first.waiting = map[string]bool{}
	first.forced = true
	heap.Push(&s.ready, first)
	return true
}

// settle takes a distribution back from its worker. What is known of the
// requirements of the distributions whose trees it collected is dropped: a
// MYMETA.json may have come with them. A target whose program was linked
// while its closure has grown since (the configure steps of the
// distributions it needs asked for more) is linked again once the new ones
// are settled; otherwise its record is final.
func (s *staticRunner) settle(a staticAttempted) error {
	n := a.job.n
	if n.forced {
		a.rec.ForcedWith = a.job.closure
	}
	n.forced = false
	for _, name := range a.collected {
		delete(s.requirements, name)
		delete(s.testRequirements, name)
	}
	if n.target && (a.rec.State == staticStatic || a.rec.State == staticNotStatic && !a.rec.moduleBuildFailed()) {
		closure, tests, err := s.closures(n)
		if err != nil {
			return s.recordUnreadable(n, err)
		}
		for _, c := range [][2][]string{{closure, a.job.closure}, {tests, a.job.testClosure}} {
			for _, name := range c[0] {
				if !slices.Contains(c[1], name) {
					fmt.Fprintf(os.Stderr, "cpancompat: %s needs %s, which a configure step asked for: linking it again\n", n.d.Dist, name)
					heap.Push(&s.ready, n)
					return nil
				}
			}
		}
	}
	return s.record(n, a.rec)
}

// recordUnreadable records a distribution whose closure cannot be read.
func (s *staticRunner) recordUnreadable(n *staticNode, err error) error {
	fmt.Fprintf(os.Stderr, "cpancompat: %s: %v\n", n.d.Dist, err)
	rec := s.newRecord(n)
	rec.State = staticDriverFailed
	return s.record(n, rec)
}

func (s *staticRunner) newRecord(n *staticNode) *staticRecord {
	return &staticRecord{Dist: n.d.Dist, Release: n.d.Release, Attempts: n.attempts}
}

// record makes a record final: it is written out, and the distributions
// waiting for this one start when it was the last they waited for.
func (s *staticRunner) record(n *staticNode, rec *staticRecord) error {
	if !n.started.IsZero() {
		rec.Seconds = int(time.Since(n.started).Seconds())
	}
	rec.Finished = time.Now().UTC().Format(time.RFC3339)
	n.rec = rec
	s.done++
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := s.results.Write(append(line, '\n')); err != nil {
		return err
	}
	detail := ""
	if len(rec.BlockedBy) > 0 {
		detail = " by " + strings.Join(rec.BlockedBy, ", ")
	}
	fmt.Fprintf(os.Stderr, "[%d/%d] %-12s %-28s %s (%ds)%s\n", s.done, s.total, rec.State, rec.Reason, n.d.Dist, rec.Seconds, detail)
	waiters := s.waiters[n.d.Dist]
	delete(s.waiters, n.d.Dist)
	for _, w := range waiters {
		if w.rec != nil || !w.waiting[n.d.Dist] {
			continue
		}
		delete(w.waiting, n.d.Dist)
		if len(w.waiting) == 0 {
			heap.Push(&s.ready, w)
		}
	}
	return nil
}

// closure returns, sorted, the distributions n's program (or, for a
// dependency, n's build) carries: n and what it needs, directly or
// indirectly. (A distribution the interpreter ships is in its own closure
// too: the snapshot pins its release, which the program then loads.)
func (s *staticRunner) closure(n *staticNode) ([]string, error) {
	return s.closureWith(n, s.requirementsOf)
}

// closures returns n's closure and, for a target, its test closure (nil for
// a dependency).
func (s *staticRunner) closures(n *staticNode) (closure, tests []string, err error) {
	if closure, err = s.closure(n); err != nil || !n.target {
		return closure, nil, err
	}
	tests, err = s.testClosure(n)
	return closure, tests, err
}

// testClosure returns, sorted, the distributions a target's test binary
// carries: its closure and what its own tests need besides, with what
// those need to be configured, built and run. A dependency's tests are
// never run (gperl's mod tidy, cpanm).
func (s *staticRunner) testClosure(n *staticNode) ([]string, error) {
	return s.closureWith(n, s.testRequirementsOf)
}

// closureWith is closure, with n's own requirements read by own.
func (s *staticRunner) closureWith(n *staticNode, own func(string) (*distRequirements, error)) ([]string, error) {
	seen := map[string]bool{n.d.Dist: true}
	stack := []string{n.d.Dist}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		of := s.requirementsOf
		if cur == n.d.Dist {
			of = own
		}
		reqs, err := of(cur)
		if err != nil {
			return nil, err
		}
		for _, need := range reqs.needs {
			if !seen[need] {
				seen[need] = true
				stack = append(stack, need)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// requirementsOf returns what the distribution needs to be configured,
// built and run: the "requires" of those phases in the MYMETA.json its
// build left in the module directory (collected into the run's trees), or
// else in its static metadata. As under cpanm and gperl
// (internal/mod/helper.pl meta), a distribution with a Build.PL that
// requires nothing to configure requires Module::Build 0.38, unless it is
// one Module::Build itself needs. A requirement the interpreter's standard
// library meets, perl itself and a module the index does not know need no
// distribution.
func (s *staticRunner) requirementsOf(name string) (*distRequirements, error) {
	return s.requirementsIn(name, staticPhases, s.requirements)
}

// testRequirementsOf is requirementsOf with the requirements of the test
// phase too: what a target's own tests need.
func (s *staticRunner) testRequirementsOf(name string) (*distRequirements, error) {
	return s.requirementsIn(name, testPhases, s.testRequirements)
}

// requirementsIn returns the requirements of the phases of the distribution,
// as requirementsOf tells them, kept in cache.
func (s *staticRunner) requirementsIn(name string, phases map[string]bool, cache map[string]*distRequirements) (*distRequirements, error) {
	if r := cache[name]; r != nil {
		return r, nil
	}
	d := s.dists[name]
	var lines []snapshotPackage
	configures := false
	mymeta := ""
	tree, err := gperlTreeName(d.Path)
	if err == nil {
		mymeta = filepath.Join(s.treesDir, tree, "MYMETA.json")
	}
	raw, err := os.ReadFile(mymeta)
	switch {
	case mymeta != "" && err == nil:
		if lines, configures, err = mymetaRequirements(raw, phases); err != nil {
			return nil, fmt.Errorf("%s: %w", mymeta, err)
		}
	case mymeta == "" || errors.Is(err, os.ErrNotExist):
		for _, req := range d.Requires {
			if req.Relationship == "requires" && phases[req.Phase] {
				lines = append(lines, snapshotPackage{req.Module, cmp.Or(req.Version, "0")})
				configures = configures || req.Phase == "configure"
			}
		}
	default:
		return nil, err
	}
	if distName, _, _ := strings.Cut(tree, "@"); mymeta != "" && !configures && !moduleBuildNeeds[distName] {
		archive, err := fetchArchive(s.opts.dataDir, d)
		if err != nil {
			return nil, err
		}
		s.verified[name] = true
		names, err := archiveNames(d.Path, archive)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		if extractedHasBuildPL(names) {
			lines = append(lines, snapshotPackage{"Module::Build", "0.38"})
		}
	}
	r := &distRequirements{lines: mergeRequirements(lines)}
	for _, line := range r.lines {
		if line.name == "perl" {
			continue
		}
		if have, ok := s.provided[line.name]; ok {
			if met, comparable := satisfies(have, line.version); comparable && met {
				continue
			}
		}
		if provider, ok := s.byModule[line.name]; ok && provider != name && !slices.Contains(r.needs, provider) {
			r.needs = append(r.needs, provider)
		}
	}
	sort.Strings(r.needs)
	cache[name] = r
	return r, nil
}

// staticPhases are the phases whose requirements a program's dependencies
// need: gperl runs neither their tests nor anyone's development tools.
var staticPhases = map[string]bool{"configure": true, "build": true, "runtime": true}

// testPhases are the phases whose requirements a target's tests need.
var testPhases = map[string]bool{"configure": true, "build": true, "runtime": true, "test": true}

// mergeRequirements gives each module one line, its ranges joined as gperl
// joins them (internal/mod/tidy.go), sorted by module.
func mergeRequirements(lines []snapshotPackage) []snapshotPackage {
	byName := map[string]string{}
	for _, l := range lines {
		have, ok := byName[l.name]
		switch {
		case !ok || have == "0":
			byName[l.name] = l.version
		case l.version != "0" && l.version != have:
			byName[l.name] = have + ", " + l.version
		}
	}
	out := make([]snapshotPackage, 0, len(byName))
	for name, v := range byName {
		out = append(out, snapshotPackage{name, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// mymetaRequirements reads the "requires" of phases of a MYMETA.json:
// CPAN::Meta::Spec version 2 (prereqs/<phase>/requires), or the version 1
// keys (configure_requires, build_requires, requires; version 1 has no
// test phase), and whether it requires anything to configure.
func mymetaRequirements(raw []byte, phases map[string]bool) ([]snapshotPackage, bool, error) {
	var meta struct {
		Spec struct {
			Version versionText `json:"version"`
		} `json:"meta-spec"`
		Prereqs           map[string]map[string]map[string]versionText `json:"prereqs"`
		Requires          map[string]versionText                       `json:"requires"`
		BuildRequires     map[string]versionText                       `json:"build_requires"`
		ConfigureRequires map[string]versionText                       `json:"configure_requires"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, false, err
	}
	var sets []map[string]versionText
	if v, ok := parsePerlVersion(string(meta.Spec.Version)); ok && v.compare(perlVersion{2}) >= 0 {
		for phase := range phases {
			sets = append(sets, meta.Prereqs[phase]["requires"])
		}
		meta.ConfigureRequires = meta.Prereqs["configure"]["requires"]
	} else {
		sets = append(sets, meta.ConfigureRequires, meta.BuildRequires, meta.Requires)
	}
	var lines []snapshotPackage
	for _, set := range sets {
		for module, v := range set {
			lines = append(lines, snapshotPackage{module, cmp.Or(string(v), "0")})
		}
	}
	return lines, len(meta.ConfigureRequires) > 0, nil
}

// moduleBuildNeeds are the distributions Module::Build depends on, which
// cpanm configures with Makefile.PL (should_use_mm; gperl's helper.pl
// %use_mm).
var moduleBuildNeeds = map[string]bool{"version": true, "ExtUtils-ParseXS": true, "ExtUtils-Install": true, "ExtUtils-Manifest": true}

// extractedHasBuildPL reports whether the tree gperl extracts an archive
// with these members into has a Build.PL: gperl drops the archive's
// top-level directory when it is the only entry at the top (internal/mod
// fetch.go extract).
func extractedHasBuildPL(names []string) bool {
	members := map[string]bool{}
	// nested says, for each top-level entry, whether anything is below it.
	nested := map[string]bool{}
	for _, n := range names {
		p := path.Clean(n)
		if p == "." {
			continue
		}
		members[p] = true
		top, _, below := strings.Cut(p, "/")
		nested[top] = nested[top] || below
	}
	file := "Build.PL"
	if len(nested) == 1 {
		for top, below := range nested {
			if below {
				file = top + "/Build.PL"
			}
		}
	}
	return members[file]
}

// gperlDistFile is how gperl splits an archive's name into the
// distribution's name and version (internal/mod/dir.go distFile), which
// name the directory it extracts the distribution into: gperl exports
// neither.
var gperlDistFile = regexp.MustCompile(`^(.+)-(v?[0-9][^-]*?)(\.tar\.gz|\.tgz|\.tar\.bz2|\.zip)$`)

// gperlTreeName returns the directory below gperl's module directory that
// the distribution at pathname (below authors/id/) is extracted into
// (gperl's mod.TreePath).
func gperlTreeName(pathname string) (string, error) {
	m := gperlDistFile.FindStringSubmatch(path.Base(pathname))
	if m == nil {
		return "", fmt.Errorf("%s: gperl takes only archives named Name-Version.tar.gz (.tgz, .tar.bz2, .zip)", pathname)
	}
	return m[1] + "@" + m[2], nil
}

// mainModules returns the modules a target's program loads: the module the
// distribution is named after when it has it, or else all its modules.
// (static.sh adds the packages of its native modules.)
func mainModules(d *dist) []string {
	if name := strings.ReplaceAll(d.Dist, "-", "::"); slices.Contains(d.Modules, name) {
		return []string{name}
	}
	return slices.Clone(d.Modules)
}

// snapshotOf writes the carton snapshot of the closure.
func (s *staticRunner) snapshotOf(closure []string) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# carton snapshot format: version 1.0\nDISTRIBUTIONS\n")
	for _, name := range closure {
		d := s.dists[name]
		reqs, err := s.requirementsOf(name)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "  %s\n    pathname: %s\n    provides:\n", d.Release, d.Path)
		for _, m := range d.Modules {
			fmt.Fprintf(&b, "      %s %s\n", m, cmp.Or(s.versions[m], "undef"))
		}
		b.WriteString("    requirements:\n")
		for _, l := range reqs.lines {
			fmt.Fprintf(&b, "      %s %s\n", l.name, l.version)
		}
	}
	return b.Bytes(), nil
}

// appArchive is the tar stream copied into a build container's root: the
// program's directory /app, owned by the image's user, with the snapshot,
// the modules to load and the trees file, and, with test, the test
// binary's snapshot, trees file and test files (static.sh).
func appArchive(snapshot []byte, load []string, trees []string, test *testInput) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	for _, dir := range []string{"app/", "app/lib/"} {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir, Mode: 0o755, Uid: imageUID, Gid: imageUID, ModTime: now}); err != nil {
			return nil, err
		}
	}
	files := []struct {
		name string
		data []byte
	}{
		{"app/lib/cpancompat-load.txt", []byte(joinLines(load))},
		{"app/trees", []byte(joinLines(trees))},
	}
	if snapshot != nil {
		files = append(files, struct {
			name string
			data []byte
		}{"app/cpanfile.snapshot", snapshot})
	}
	if test != nil {
		files = append(files, []struct {
			name string
			data []byte
		}{
			{"app/test-cpanfile.snapshot", test.snapshot},
			{"app/test-trees", []byte(joinLines(test.trees))},
			{"app/test-files", []byte(joinLines(test.files))},
		}...)
	}
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.name, Mode: 0o644, Size: int64(len(f.data)), Uid: imageUID, Gid: imageUID, ModTime: now}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// imageUID is the user and group of the image's user (user.sh).
const imageUID = 1000

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// staticJob is one container run of a distribution, prepared by the
// coordinating goroutine.
type staticJob struct {
	n        *staticNode
	closure  []string
	attempts int
	seq      int
	// own is the distribution's tree.
	own string
	// treeDist maps the trees of the closure to their distributions.
	treeDist map[string]string
	// load are the modules the program loads (none for a dependency).
	load  []string
	input []byte
	// testClosure is a target's test closure (nil for a dependency), and
	// testFiles the test files its test binary runs. test is how its tests
	// went when that is known before its container runs: the run recorded
	// no test files, or a distribution only they need was not built.
	testClosure []string
	testFiles   []string
	test        *staticTest
}

// testInput is what a build container gets for a target's test binary: the
// snapshot of its test closure, the trees file of the distributions only
// its tests need, and the test files to run.
type testInput struct {
	snapshot []byte
	trees    []string
	files    []string
}

// prepare makes the job of n's next container: the archives of the closure
// in the data directory, and the program's directory, with the snapshot of
// the closure and the trees file. The trees file lists the trees of the
// closure, the distribution's own first, with whether to collect what gperl
// derives from each: the ones the run has not collected yet, which only
// this container can build (every other is settled, see waitFor). A
// target's container also gets what its test binary is made from
// (testInput), unless its test state is known already.
func (s *staticRunner) prepare(n *staticNode, closure, tests []string, seq int) (*staticJob, error) {
	job := &staticJob{n: n, closure: closure, attempts: n.attempts, seq: seq, own: "-", treeDist: map[string]string{},
		testClosure: tests, testFiles: s.testFiles[n.d.Dist]}
	var trees []string
	for _, name := range closure {
		d := s.dists[name]
		if err := s.verifyArchive(d); err != nil {
			return nil, err
		}
		tree, err := gperlTreeName(d.Path)
		if err != nil {
			return nil, err
		}
		job.treeDist[tree] = name
		collect := "0"
		if _, err := os.Stat(filepath.Join(s.treesDir, tree)); errors.Is(err, os.ErrNotExist) {
			collect = "1"
		} else if err != nil {
			return nil, err
		}
		line := tree + "\t" + d.Path + "\t" + collect
		if name == n.d.Dist {
			job.own = tree
			trees = append([]string{line}, trees...)
		} else {
			trees = append(trees, line)
		}
	}
	snapshot, err := s.snapshotOf(closure)
	if err != nil {
		return nil, err
	}
	var test *testInput
	if n.target {
		job.load = mainModules(n.d)
		if test, err = s.testInputOf(job); err != nil {
			return nil, err
		}
	}
	if job.input, err = appArchive(snapshot, job.load, trees, test); err != nil {
		return nil, err
	}
	return job, nil
}

// testInputOf returns what a target's container makes its test binary from;
// nil, with job.test set, when its tests cannot run: the run recorded no
// test files of it, or a distribution only its tests need is not built.
// That is one whose build failed, or one not settled yet: a target forced
// to start (see force) waits for none, and its container must not build a
// distribution whose record is another's; such a target's tests are not
// run, and name the distributions that were not built when it started.
func (s *staticRunner) testInputOf(job *staticJob) (*testInput, error) {
	if len(job.testFiles) == 0 {
		job.test = &staticTest{State: testNoRecord}
		return nil, nil
	}
	in := &testInput{files: job.testFiles}
	var blocked []string
	for _, name := range job.testClosure {
		if slices.Contains(job.closure, name) {
			continue
		}
		if m := s.nodes[name]; m == nil || m.rec == nil || m.rec.moduleBuildFailed() {
			blocked = append(blocked, name)
			continue
		}
		d := s.dists[name]
		if err := s.verifyArchive(d); err != nil {
			return nil, err
		}
		tree, err := gperlTreeName(d.Path)
		if err != nil {
			return nil, err
		}
		// Its own container collected it.
		in.trees = append(in.trees, tree+"\t"+d.Path+"\t0")
	}
	if len(blocked) > 0 {
		job.test = &staticTest{State: testDepsFailed, Files: len(job.testFiles), BlockedBy: blocked}
		return nil, nil
	}
	var err error
	in.snapshot, err = s.snapshotOf(job.testClosure)
	return in, err
}

// attempt measures one distribution once: it builds the closure in the
// module directory and, for a target, links the program and runs it. It
// runs on a worker goroutine and changes nothing shared but the files of
// the distribution and of the trees it collects (no other worker collects
// those, see prepare). It returns the record and the distributions whose
// trees it collected.
func (s *staticRunner) attempt(job *staticJob) (*staticRecord, []string) {
	n := job.n
	rec := &staticRecord{Dist: n.d.Dist, Release: n.d.Release, Attempts: job.attempts, Closure: job.closure}
	var collected []string
	fail := func(err error) (*staticRecord, []string) {
		rec.State = staticDriverFailed
		fmt.Fprintf(os.Stderr, "cpancompat: %s: %v\n", n.d.Dist, err)
		return rec, collected
	}
	mode := "deps"
	if n.target {
		mode = "app"
	}
	dir := filepath.Join(s.staticDir, "dists", n.d.Dist)
	if err := os.RemoveAll(dir); err != nil {
		return fail(err)
	}
	name := fmt.Sprintf("cpancompat-%s-static-%d", s.opts.run, job.seq)
	b, err := s.build(name, dir, mode, job.own, job.input, true)
	if err != nil {
		return fail(err)
	}
	defer b.remove()
	if rec.Phases, err = readJSONLines[phaseRecord](filepath.Join(dir, "phases.jsonl")); err != nil {
		return fail(err)
	}
	states, err := readTreeStates(filepath.Join(dir, "trees.tsv"))
	if err != nil {
		return fail(err)
	}
	moved, err := s.keepTrees(dir, job.treeDist)
	for _, tree := range moved {
		collected = append(collected, job.treeDist[tree])
	}
	if err != nil {
		return fail(err)
	}
	natives, err := os.ReadFile(filepath.Join(dir, "natives.txt"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	rec.Loaded = mergeLoaded(job.load, strings.Fields(string(natives)))

	modules := phaseNamed(rec.Phases, "modules")
	if modules == nil {
		return fail(fmt.Errorf("the build container recorded no modules phase (%s)", filepath.Join(dir, "container.log")))
	}
	if modules.Status != 0 {
		rec.State, rec.Reason, rec.BlockedBy = modulesFailure(job.own, states, job.treeDist, modules.TimedOut)
		if rec.State == "" {
			return fail(fmt.Errorf("the modules phase failed with every tree built (%s)", filepath.Join(dir, "modules.log")))
		}
		if !n.target {
			// A dependency that could not be built blocks what needs it.
			rec.State, rec.Reason = staticDependency, cmp.Or(rec.Reason, reasonDistBuildFailed)
		}
		return rec, collected
	}
	if !n.target {
		rec.State = staticDependency
		return rec, collected
	}
	mods, err := s.closureModules(job.closure)
	if err != nil {
		return fail(err)
	}
	for _, m := range mods {
		rec.NativeModules = append(rec.NativeModules, staticNative{Package: m.pkg, Dist: m.dist})
	}
	rec.ExternalLibs = externalLibraries(mods, s.probe.archives)
	link := phaseNamed(rec.Phases, "link")
	if link == nil {
		return fail(fmt.Errorf("the build container recorded no link phase (%s)", filepath.Join(dir, "container.log")))
	}
	rec.State = staticNotStatic
	if link.TimedOut {
		rec.Reason = reasonLinkTimeout
		return rec, collected
	}
	if link.Status != 0 {
		rec.Reason, rec.CausedBy = linkFailure(n.d.Dist, mods, s.probe.runtime, rec.ExternalLibs)
		return rec, collected
	}
	bin := filepath.Join(dir, ".app")
	if err := copyFileOut(name, "/bin-out/app", bin); err != nil {
		return fail(err)
	}
	info, err := binaryOf(bin)
	if rmErr := os.Remove(bin); rmErr != nil {
		err = errors.Join(err, rmErr)
	}
	if err != nil {
		return fail(err)
	}
	rec.Binary = &info
	if info.Interp != "" || len(info.Needed) > 0 {
		rec.Reason = reasonNotFullyStatic
		return rec, collected
	}
	empty, err := s.runBinary(name+"-run", b.volume, filepath.Join(dir, "run_empty.log"), true)
	if err != nil {
		return fail(err)
	}
	rec.Phases = append(rec.Phases, empty)
	switch {
	case empty.Status == 0:
		rec.State = staticStatic
		if len(rec.ExternalLibs) > 0 {
			rec.Reason = reasonBundlesSystemLibraries
		}
		s.runTests(job, name, b.volume, dir, rec, s.runIn)
		return rec, collected
	case empty.TimedOut:
		rec.Reason = reasonLoadTimeout
		return rec, collected
	}
	control, err := s.runBinary(name+"-control", b.volume, filepath.Join(dir, "run_control.log"), false)
	if err != nil {
		return fail(err)
	}
	rec.Phases = append(rec.Phases, control)
	rec.Reason = reasonLoadFailed
	if control.Status == 0 {
		rec.Reason = reasonNeedsEnvironment
	}
	return rec, collected
}

// mergeLoaded returns the modules a program loads, in its order, once each.
func mergeLoaded(main, natives []string) []string {
	var out []string
	for _, m := range append(slices.Clone(main), natives...) {
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// phaseNamed returns the record of the named phase, or nil.
func phaseNamed(phases []phaseRecord, name string) *phaseRecord {
	for i := range phases {
		if phases[i].Phase == name {
			return &phases[i]
		}
	}
	return nil
}

// The state of a tree in the module directory once a build container is
// done (static.sh writes trees.tsv).
const (
	// treeBuilt: gperl marked it built.
	treeBuilt = "built"
	// treeFailed: its build failed, and gperl kept the log.
	treeFailed = "failed"
	// treeRefused: gperl kept what the build's configure step settled on
	// (MYMETA.json, which it copies once the build has succeeded) and
	// marked it neither built nor failed: it refused the native modules
	// the build made.
	treeRefused = "refused"
	// treeUnbuilt: extracted, and none of that.
	treeUnbuilt = "unbuilt"
	treeAbsent  = "absent"
)

// readTreeStates reads trees.tsv: one "tree<TAB>state" line per tree.
func readTreeStates(name string) (map[string]string, error) {
	raw, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	states := map[string]string{}
	for i, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		tree, state, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("%s: line %d: want tree and state", name, i+1)
		}
		switch state {
		case treeBuilt, treeFailed, treeRefused, treeUnbuilt, treeAbsent:
		default:
			return nil, fmt.Errorf("%s: line %d: unknown state %q", name, i+1, state)
		}
		states[tree] = state
	}
	return states, nil
}

// modulesFailure says why the modules phase of a container failed, by its
// record and the state of the trees once it was over: it ran out of time;
// the distribution's own build failed (gperl kept its log); gperl refused
// what it built; the build of another distribution of the closure failed in
// this container (one it was forced to build, see force), which blocks it;
// or gperl failed on the distribution before its build kept a log (its
// message is in the phase's log). treeDist maps the closure's trees to
// their distributions. It returns no state when the distribution was built
// all the same.
func modulesFailure(own string, states, treeDist map[string]string, timedOut bool) (state, reason string, blockedBy []string) {
	var notBuilt []string
	for tree, st := range states {
		if tree == own || st == treeBuilt {
			continue
		}
		notBuilt = append(notBuilt, treeDist[tree])
		if st == treeFailed || st == treeRefused {
			blockedBy = append(blockedBy, treeDist[tree])
		}
	}
	sort.Strings(blockedBy)
	sort.Strings(notBuilt)
	switch {
	case states[own] == treeBuilt:
		// Built: what is left unbuilt of the closure (in a forced
		// container) blocks it.
		if len(notBuilt) > 0 {
			return staticBlocked, "", notBuilt
		}
		return "", "", nil
	case timedOut:
		return staticNotStatic, reasonModulesTimeout, nil
	case states[own] == treeFailed:
		return staticNotStatic, reasonDistBuildFailed, nil
	case states[own] == treeRefused:
		return staticNotStatic, reasonLinkerOverridden, nil
	case len(blockedBy) > 0:
		return staticBlocked, "", blockedBy
	}
	return staticNotStatic, reasonDistBuildFailed, nil
}

// keepTrees moves what the container collected (dir/trees/<tree>) of the
// closure's trees into the run's trees, and returns the trees it moved.
// Anything else there is not gperl's: the distributions built in the
// container could write below /out.
func (s *staticRunner) keepTrees(dir string, treeDist map[string]string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "trees"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var moved []string
	for _, e := range entries {
		if _, ok := treeDist[e.Name()]; !ok || !e.IsDir() {
			continue
		}
		dst := filepath.Join(s.treesDir, e.Name())
		if err := os.RemoveAll(dst); err != nil {
			return moved, err
		}
		if err := os.Rename(filepath.Join(dir, "trees", e.Name()), dst); err != nil {
			return moved, err
		}
		moved = append(moved, e.Name())
	}
	return moved, os.RemoveAll(filepath.Join(dir, "trees"))
}

// verifyArchive makes the distribution's archive present in the data
// directory, checked against its recorded checksum, once per invocation.
func (s *staticRunner) verifyArchive(d *dist) error {
	if s.verified[d.Dist] {
		return nil
	}
	if _, err := fetchArchive(s.opts.dataDir, d); err != nil {
		return err
	}
	s.verified[d.Dist] = true
	return nil
}

// builtContainer is a build container that has ended, and its binary
// volume.
type builtContainer struct {
	name, volume string
}

// remove removes the container and the volume, reporting a failure: what
// it leaves, the next invocation removes (prepareDocker).
func (b builtContainer) remove() {
	if err := removeContainers(b.name, b.name+"-run", b.name+"-control", b.name+"-test", b.name+"-test-control", b.name+"-test-dynamic"); err != nil {
		fmt.Fprintf(os.Stderr, "cpancompat: %v\n", err)
	}
	if out, err := exec.Command("docker", "volume", "rm", b.volume).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "cpancompat: docker volume rm %s: %v: %s\n", b.volume, err, out)
	}
}

// build runs one build container (static.sh mode) with input copied into
// its root, and copies its records into dir, with what docker printed in
// dir/container.log. The container and its binary volume are left for the
// caller to use and remove; they are removed here when this fails.
func (s *staticRunner) build(name, dir, mode, own string, input []byte, offlineGo bool) (b builtContainer, err error) {
	b = builtContainer{name: name, volume: name + "-bin"}
	if out, err := exec.Command("docker", "volume", "create", "--label", s.label, b.volume).CombinedOutput(); err != nil {
		return b, fmt.Errorf("docker volume create: %v: %s", err, out)
	}
	defer func() {
		if err != nil {
			b.remove()
		}
	}()
	work := filepath.Dir(dir)
	log, err := os.CreateTemp(work, "."+filepath.Base(dir)+".container.log.")
	if err != nil {
		return b, err
	}
	// The log is renamed into dir once the records are there.
	defer func() {
		if rmErr := os.Remove(log.Name()); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
	}()
	// Each phase of the container has its time limit (static.sh); what it
	// does besides is copying files.
	args := append(s.buildArgs(b.volume, offlineGo), s.cacheImage, "bash", "/opt/cpancompat/static.sh",
		mode, strconv.Itoa(int(s.opts.wall/time.Second)), own)
	end, _, runErr := runContainer(name, s.label, args, input, log, 0)
	if err := log.Close(); err != nil {
		return b, errors.Join(runErr, err)
	}
	if runErr != nil {
		return b, runErr
	}
	if err := copyOut(name, dir); err != nil {
		return b, err
	}
	if err := os.Rename(log.Name(), filepath.Join(dir, "container.log")); err != nil {
		return b, err
	}
	if end.ExitCode != 0 {
		return b, fmt.Errorf("the build container ended with status %d%s (%s)", end.ExitCode, end.errorText(),
			filepath.Join(dir, "container.log"))
	}
	return b, nil
}

// runBinary runs the binary of a build container's volume: in the empty
// container, or (empty false) in the build image, as the image's user, with
// everything the build had but the network. Its output goes to logName.
func (s *staticRunner) runBinary(name, volume, logName string, empty bool) (phaseRecord, error) {
	r := binaryRun{phase: "run_control", empty: empty, argv: []string{"/app/app"}, limit: loadLimit, memory: loadMemory,
		pids: "512", mounts: []string{"-v", volume + ":/app:ro"}}
	if empty {
		r.phase = "run_empty"
	}
	return s.runIn(name, logName, r)
}

// binaryRun is one run of a program in a container without the network.
type binaryRun struct {
	phase string
	// empty: in the empty container, as loadUser; otherwise in the build
	// image, as its user.
	empty bool
	// mounts are the container's volumes (docker create arguments), input
	// a tar stream copied into its root.
	mounts []string
	input  []byte
	// argv is its command line, env what it adds to the environment.
	argv []string
	env  []string
	// limit is how long it may run; memory and pids are the limits of its
	// container.
	limit        time.Duration
	memory, pids string
	// init runs it below an init process, which reaps the processes the
	// program's children leave.
	init bool
	// stuck, when not "", is the file a record of the container's
	// processes goes to (watch.sh snapshot) when it still runs 15 seconds
	// before its limit: what tells why a program ran out of time.
	stuck string
}

// runIn runs a container as r says, with its output in logName.
func (s *staticRunner) runIn(name, logName string, r binaryRun) (phaseRecord, error) {
	p := phaseRecord{Phase: r.phase}
	args := append([]string{"--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--ulimit", "core=0", "--memory", r.memory, "--pids-limit", r.pids, "--tmpfs", loadTmpfs}, r.mounts...)
	if r.init {
		args = append(args, "--init")
	}
	for _, kv := range r.env {
		args = append(args, "-e", kv)
	}
	if r.empty {
		args = append(args, "--user", loadUser, "--read-only", emptyImage)
	} else {
		args = append(args, s.cacheImage)
	}
	args = append(args, r.argv...)
	log, err := os.Create(logName)
	if err != nil {
		return p, err
	}
	var snapshot func() error
	if r.stuck != "" && r.limit > 30*time.Second {
		snapshot = func() error { return s.snapshotProcesses(name, r.stuck) }
	}
	start := time.Now()
	end, timedOut, snapshotErr, err := superviseRun(r.limit, r.limit-15*time.Second, snapshot,
		func() error { return exec.Command("docker", "kill", name).Run() },
		func() (containerEnd, error) {
			end, _, err := runContainer(name, s.label, args, r.input, log, 0)
			return end, err
		})
	p.Seconds = int(time.Since(start).Seconds())
	if snapshotErr != nil {
		// Without the record, a program that ran out of time is not told
		// blocked, spinning or slow; that is all it changes.
		fmt.Fprintf(os.Stderr, "cpancompat: the record of the processes of %s: %v\n", name, snapshotErr)
	}
	if err = errors.Join(err, log.Close()); err != nil {
		return p, err
	}
	p.Status, p.TimedOut = end.ExitCode, timedOut
	if end.Error != "" && p.Status == 0 {
		// The runtime could not start the program.
		p.Status = -1
	}
	return p, nil
}

// superviseRun runs a container by run, which returns when the container
// has ended, and ends it by kill once it has run for limit (timedOut). With
// snapshot, a record of its processes is taken at snapshotAt while it runs,
// and the kill at the limit waits for that record to be whole: the record
// is of a program that ran out of time, taken before it was ended.
func superviseRun(limit, snapshotAt time.Duration, snapshot, kill func() error, run func() (containerEnd, error)) (end containerEnd, timedOut bool, snapshotErr, err error) {
	start := time.Now()
	done := make(chan struct{})
	var killErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// wait waits until d after the start, and reports whether the
		// container still runs then.
		wait := func(d time.Duration) bool {
			t := time.NewTimer(d - time.Since(start))
			defer t.Stop()
			select {
			case <-done:
				return false
			case <-t.C:
				return true
			}
		}
		if snapshot != nil {
			if !wait(snapshotAt) {
				return
			}
			snapshotErr = snapshot()
		}
		if !wait(limit) {
			return
		}
		timedOut = true
		killErr = kill()
	}()
	end, err = run()
	close(done)
	wg.Wait()
	if killErr != nil {
		// The container has ended (run returned): a kill that failed found
		// it ended already, or it ended otherwise.
		fmt.Fprintf(os.Stderr, "cpancompat: ending a container at its limit: %v\n", killErr)
	}
	return end, timedOut, snapshotErr, err
}

// snapshotProcesses writes to out what the processes of the running
// container name are doing (watch.sh snapshot), from a container of the
// build image that shares its processes: the container itself may hold
// nothing but a binary. It is the container's user, which may read what its
// processes have open.
func (s *staticRunner) snapshotProcesses(name, out string) error {
	partial := out + ".partial"
	f, err := os.Create(partial)
	if err != nil {
		return err
	}
	cmd := exec.Command("docker", "run", "--rm", "--name", name+"-watch", "--label", s.label, "--pid", "container:"+name,
		"--init", "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--ulimit", "core=0",
		"--user", loadUser, s.cacheImage, "bash", "/opt/cpancompat/watch.sh", "snapshot")
	var stderr strings.Builder
	cmd.Stdout, cmd.Stderr = f, &stderr
	err = cmd.Run()
	if err = errors.Join(err, f.Close()); err != nil {
		return errors.Join(fmt.Errorf("docker run: %v: %s", err, stderr.String()), os.Remove(partial))
	}
	return os.Rename(partial, out)
}

// runTests runs a static target's own test files with its test binary (the
// build container's test_link) and records how they went in rec.Test: in
// the empty container; when they fail there, with the same binary in the
// build image, which has what the run's tests had but the network; and when
// they fail there too, on the same project with gperl test, in the same
// conditions. Each container runs by run (runIn). Nothing here changes the
// record of the program: a test run that cannot be carried out (the
// harness or docker failing) leaves rec.Test nil and says why on standard
// error.
func (s *staticRunner) runTests(job *staticJob, name, volume, dir string, rec *staticRecord, run func(name, logName string, r binaryRun) (phaseRecord, error)) {
	if job.test != nil {
		rec.Test = job.test
		return
	}
	t, err := s.testOutcome(job, name, volume, dir, rec, run)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cpancompat: %s: its tests could not be run: %v\n", rec.Dist, err)
		return
	}
	rec.Test = t
}

// testOutcome is how a static target's tests went (runTests).
func (s *staticRunner) testOutcome(job *staticJob, name, volume, dir string, rec *staticRecord, run func(name, logName string, r binaryRun) (phaseRecord, error)) (*staticTest, error) {
	t := &staticTest{Files: len(job.testFiles)}
	for _, step := range []struct{ phase, state string }{{"test_modules", testDepsFailed}, {"test_link", testLinkFailed}} {
		p := phaseNamed(rec.Phases, step.phase)
		if p == nil {
			return nil, fmt.Errorf("the build container recorded no %s phase (%s)", step.phase, filepath.Join(dir, "container.log"))
		}
		if p.Status != 0 || p.TimedOut {
			t.State = step.state
			return t, nil
		}
	}
	argv := append([]string{testBinaryPath, "-j", "1"}, job.testFiles...)
	binary := []string{"-v", volume + ":/app:ro"}
	r := binaryRun{phase: "test_empty", empty: true, mounts: binary, argv: argv, env: testEnvironment, limit: s.testLimit,
		memory: s.testMemory, pids: testPids, init: true, stuck: filepath.Join(dir, "test_empty.stuck.jsonl")}
	empty, err := run(name+"-test", filepath.Join(dir, "test_empty.log"), r)
	if err != nil {
		return nil, err
	}
	rec.Phases = append(rec.Phases, empty)
	switch {
	case empty.Status == 0:
		t.State = testPass
		return t, nil
	case empty.TimedOut:
		t.State = testTimeout
		if st := stuckOf(r.stuck, testBinaryPath); st != nil {
			t.Stuck = st.Kind
		}
		return t, nil
	}
	// The run's tests had a display (dist.sh); gperl test runs on the
	// module directory, and builds nothing (the test binary was built).
	control := binaryRun{phase: "test_control", mounts: binary, argv: append([]string{"xvfb-run", "-a"}, argv...),
		env: testEnvironment, limit: s.testLimit, memory: s.testMemory, pids: testPids, init: true}
	dynamic := control
	dynamic.phase, dynamic.input = "test_dynamic", job.input
	dynamic.mounts = []string{"-v", s.modVolume + ":/opt/gperl/mod",
		"--tmpfs", "/opt/gperl/mod/cache/sdk:rw,mode=1777", "--tmpfs", "/opt/gperl/mod/cache/linkset:rw,mode=1777",
		"--tmpfs", "/opt/gperl/mod/cache/lib:rw,exec,mode=1777"}
	dynamic.argv = []string{"xvfb-run", "-a", "bash", "/opt/cpancompat/static.sh", "test-dynamic", job.own}
	for _, c := range []struct {
		r     binaryRun
		name  string
		state string
	}{{control, name + "-test-control", testNeedsEnvironment}, {dynamic, name + "-test-dynamic", testStaticFailed}} {
		p, err := run(c.name, filepath.Join(dir, c.r.phase+".log"), c.r)
		if err != nil {
			return nil, err
		}
		rec.Phases = append(rec.Phases, p)
		if p.Status == 0 && !p.TimedOut {
			t.State = c.state
			return t, nil
		}
	}
	t.State = testDiffersFromMakeTest
	return t, nil
}

// containerEnd is how a container ended (docker inspect's State).
type containerEnd struct {
	Running   bool   `json:"Running"`
	ExitCode  int    `json:"ExitCode"`
	OOMKilled bool   `json:"OOMKilled"`
	Error     string `json:"Error"`
}

func (e containerEnd) errorText() string {
	switch {
	case e.Error != "":
		return ": " + e.Error
	case e.OOMKilled:
		return ", out of memory"
	}
	return ""
}

// runContainer creates the container name with args (docker create's,
// after the name), labelled with label, copies input (a tar stream) into
// its root when given, and runs it with its output in log until it ends or,
// when limit is not 0, has run for limit, when it is killed. It reports how
// the container ended; err is a failure of docker itself. The container is
// left for the caller.
func runContainer(name, label string, args []string, input []byte, log io.Writer, limit time.Duration) (end containerEnd, timedOut bool, err error) {
	create := append([]string{"create", "--name", name, "--label", label}, args...)
	if out, err := exec.Command("docker", create...).CombinedOutput(); err != nil {
		return end, false, fmt.Errorf("docker create: %v: %s", err, out)
	}
	if input != nil {
		cp := exec.Command("docker", "cp", "--archive", "-", name+":/")
		cp.Stdin = bytes.NewReader(input)
		if out, err := cp.CombinedOutput(); err != nil {
			return end, false, fmt.Errorf("docker cp: %v: %s", err, out)
		}
	}
	ctx := context.Background()
	if limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	start := exec.CommandContext(ctx, "docker", "start", "--attach", name)
	// Whether the limit ended the container is whether it was killed for
	// it, not whether the limit has passed when docker start returns.
	start.Cancel = func() error {
		timedOut = true
		return exec.Command("docker", "kill", name).Run()
	}
	start.Stdout, start.Stderr = log, log
	// The exit status of docker start is the container's, read below. Once
	// the limit has passed, what killing it did is told by its state too.
	startErr := start.Run()
	out, err := exec.Command("docker", "inspect", "--format", "{{json .State}}", name).Output()
	if err != nil {
		return end, timedOut, errors.Join(fmt.Errorf("docker inspect: %w", err), startErr)
	}
	if err := json.Unmarshal(out, &end); err != nil {
		return end, timedOut, fmt.Errorf("docker inspect: %w", err)
	}
	if end.Running {
		return end, timedOut, errors.Join(fmt.Errorf("container %s is still running", name), startErr)
	}
	var exitErr *exec.ExitError
	if startErr != nil && !timedOut && !errors.As(startErr, &exitErr) {
		return end, timedOut, fmt.Errorf("docker start: %w", startErr)
	}
	return end, timedOut, nil
}

// removeContainers removes the named containers that exist.
func removeContainers(names ...string) error {
	args := []string{"ps", "--all", "--quiet"}
	for _, n := range names {
		args = append(args, "--filter", "name=^"+n+"$")
	}
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		return fmt.Errorf("docker ps: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	if out, err := exec.Command("docker", append([]string{"rm", "--force"}, ids...)...).CombinedOutput(); err != nil {
		return fmt.Errorf("docker rm: %v: %s", err, out)
	}
	return nil
}

// copyFileOut copies the regular file src of the container name to dst,
// which must not exist. docker cp hands it over as a tar stream of that one
// file.
func copyFileOut(name, src, dst string) error {
	cmd := exec.Command("docker", "cp", name+":"+src, "-")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stream, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	copyErr := func() error {
		tr := tar.NewReader(stream)
		h, err := tr.Next()
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || h.Name != path.Base(src) {
			return fmt.Errorf("docker cp %s: not a regular file", src)
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		return errors.Join(err, f.Close())
	}()
	// The rest of the stream, so that docker cp can end.
	if _, err := io.Copy(io.Discard, stream); err != nil {
		copyErr = errors.Join(copyErr, err)
	}
	if err := cmd.Wait(); err != nil {
		return errors.Join(fmt.Errorf("docker cp: %v: %s", err, stderr.String()), copyErr)
	}
	return copyErr
}
