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

import (
	"archive/tar"
	"bytes"
	"cmp"
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
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

// The conditions of the empty container a binary is run in.
const (
	// emptyImage is the image without a single file (cpancompat image
	// -static makes it).
	emptyImage = "cpancompat-empty:latest"
	// loadLimit is how long a binary may run there.
	loadLimit = 120 * time.Second
	// loadMemory is the memory of that container, the survey's.
	loadMemory = "1500m"
	// loadTmpfs is the writable /tmp it gets.
	loadTmpfs = "/tmp:rw,size=1g,mode=1777"
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
	// modVolume is gperl's module directory, goVolume the Go build and
	// module caches; both are shared by all containers.
	modVolume string
	goVolume  string
	label     string
	results   *os.File

	probe *staticProbe
	// requirements caches what each distribution needs, as far as is
	// known (requirementsOf).
	requirements map[string]*distRequirements
	// verified are the distributions whose archives are in the data
	// directory, checked.
	verified map[string]bool
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
		goVolume:     "cpancompat-" + opts.run + "-static-go",
		label:        "cpancompat-static=" + opts.run,
		requirements: map[string]*distRequirements{}, verified: map[string]bool{},
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
	for _, v := range []string{s.modVolume, s.goVolume} {
		if out, err := exec.Command("docker", "volume", "create", v).CombinedOutput(); err != nil {
			return fmt.Errorf("docker volume create: %v: %s", err, out)
		}
	}
	// Docker fills an empty volume from the image the first time a
	// container mounts it (see runSurvey). The directories each container
	// mounts a tmpfs of its own over are made here, as the user: Docker
	// would make a missing one as root, and the module directory's cache
	// directory with it, where gperl then could not write.
	if out, err := exec.Command("docker", "run", "--rm", "--network", "none",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--ulimit", "core=0",
		"-v", s.modVolume+":/opt/gperl/mod", "-v", s.goVolume+":/opt/go-cache",
		s.opts.image, "mkdir", "-p", "/opt/gperl/mod/cache/sdk", "/opt/gperl/mod/cache/linkset",
		"/opt/gperl/mod/cache/lib", "/opt/gperl/mod/cache/download").CombinedOutput(); err != nil {
		return fmt.Errorf("filling the volumes from %s: %v: %s", s.opts.image, err, out)
	}
	return guardNetwork(s.opts.dataDir)
}

// buildArgs are the docker create arguments of a build container, up to
// the image: the shared volumes, a binary volume of its own at /bin-out,
// and its own tmpfs where gperl writes per process (the SDK headers, which
// it writes again for every build) or per program (the link set and the
// merged module tree), so that containers neither change one another's
// files nor leave theirs behind.
func (s *staticRunner) buildArgs(binVolume string, offlineGo bool) []string {
	args := []string{
		"--init", "--network", surveyNetwork,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--ulimit", "core=0",
		"--cpus", s.opts.cpus, "--memory", s.opts.memory, "--pids-limit", "4096",
		"-v", s.modVolume + ":/opt/gperl/mod",
		"-v", s.goVolume + ":/opt/go-cache",
		"-v", binVolume + ":/bin-out",
		"-v", s.archives + ":/archives:ro",
		"--tmpfs", "/opt/gperl/mod/cache/sdk:rw,mode=1777",
		"--tmpfs", "/opt/gperl/mod/cache/linkset:rw,mode=1777",
		"--tmpfs", "/opt/gperl/mod/cache/lib:rw,mode=1777",
	}
	if offlineGo {
		// The Go modules every program needs are in the module cache
		// since warm.
		args = append(args, "-e", "GOPROXY=off")
	}
	return args
}

// staticProbe is what the image says about its toolchain.
type staticProbe struct {
	// searchDirs are the compiler's library directories, in its order.
	searchDirs []string
	// archives maps the name of a static archive in them to its path in
	// the first directory that has it.
	archives map[string]string
	runtime  runtimeSymbols
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
	input, err := appArchive(nil, nil, nil)
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
			closure, err := s.closure(n)
			if err != nil {
				if err := s.recordUnreadable(n, err); err != nil {
					return err
				}
				continue
			}
			if s.waitFor(n, closure) {
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
			job, err := s.prepare(n, closure, seq)
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
	}
	if n.target && (a.rec.State == staticStatic || a.rec.State == staticNotStatic && !a.rec.moduleBuildFailed()) {
		closure, err := s.closure(n)
		if err != nil {
			return s.recordUnreadable(n, err)
		}
		for _, name := range closure {
			if !slices.Contains(a.job.closure, name) {
				fmt.Fprintf(os.Stderr, "cpancompat: %s needs %s, which a configure step asked for: linking it again\n", n.d.Dist, name)
				heap.Push(&s.ready, n)
				return nil
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
	seen := map[string]bool{n.d.Dist: true}
	stack := []string{n.d.Dist}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		reqs, err := s.requirementsOf(cur)
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
// else in its static metadata. A requirement the interpreter's standard
// library meets, perl itself and a module the index does not know need no
// distribution.
func (s *staticRunner) requirementsOf(name string) (*distRequirements, error) {
	if r := s.requirements[name]; r != nil {
		return r, nil
	}
	d := s.dists[name]
	var lines []snapshotPackage
	mymeta := ""
	if tree, err := gperlTreeName(d.Path); err == nil {
		mymeta = filepath.Join(s.treesDir, tree, "MYMETA.json")
	}
	raw, err := os.ReadFile(mymeta)
	switch {
	case mymeta != "" && err == nil:
		if lines, err = mymetaRequirements(raw); err != nil {
			return nil, fmt.Errorf("%s: %w", mymeta, err)
		}
	case mymeta == "" || errors.Is(err, os.ErrNotExist):
		for _, req := range d.Requires {
			if req.Relationship == "requires" && staticPhases[req.Phase] {
				lines = append(lines, snapshotPackage{req.Module, cmp.Or(req.Version, "0")})
			}
		}
	default:
		return nil, err
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
	s.requirements[name] = r
	return r, nil
}

// staticPhases are the phases whose requirements a program's dependencies
// need: gperl runs neither their tests nor anyone's development tools.
var staticPhases = map[string]bool{"configure": true, "build": true, "runtime": true}

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

// mymetaRequirements reads the configure, build and runtime "requires" of a
// MYMETA.json: CPAN::Meta::Spec version 2 (prereqs/<phase>/requires), or
// the version 1 keys (configure_requires, build_requires, requires).
func mymetaRequirements(raw []byte) ([]snapshotPackage, error) {
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
		return nil, err
	}
	var sets []map[string]versionText
	if v, ok := parsePerlVersion(string(meta.Spec.Version)); ok && v.compare(perlVersion{2}) >= 0 {
		for phase := range staticPhases {
			sets = append(sets, meta.Prereqs[phase]["requires"])
		}
	} else {
		sets = append(sets, meta.ConfigureRequires, meta.BuildRequires, meta.Requires)
	}
	var lines []snapshotPackage
	for _, set := range sets {
		for module, v := range set {
			lines = append(lines, snapshotPackage{module, cmp.Or(string(v), "0")})
		}
	}
	return lines, nil
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
// the modules to load and the trees file.
func appArchive(snapshot []byte, load []string, trees []string) ([]byte, error) {
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
}

// prepare makes the job of n's next container: the archives of the closure
// in the data directory, and the program's directory, with the snapshot of
// the closure and the trees file. The trees file lists the trees of the
// closure, the distribution's own first, with whether to collect what gperl
// derives from each: the ones the run has not collected yet, which only
// this container can build (every other is settled, see waitFor).
func (s *staticRunner) prepare(n *staticNode, closure []string, seq int) (*staticJob, error) {
	job := &staticJob{n: n, closure: closure, attempts: n.attempts, seq: seq, own: "-", treeDist: map[string]string{}}
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
	if n.target {
		job.load = mainModules(n.d)
	}
	if job.input, err = appArchive(snapshot, job.load, trees); err != nil {
		return nil, err
	}
	return job, nil
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
	if err := removeContainers(b.name, b.name+"-run", b.name+"-control"); err != nil {
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
	args := append(s.buildArgs(b.volume, offlineGo), s.opts.image, "bash", "/opt/cpancompat/static.sh",
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
	p := phaseRecord{Phase: "run_control"}
	args := []string{"--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--ulimit", "core=0", "--memory", loadMemory, "--pids-limit", "512", "--tmpfs", loadTmpfs,
		"-v", volume + ":/app:ro"}
	if empty {
		p.Phase = "run_empty"
		args = append(args, "--user", loadUser, "--read-only", emptyImage, "/app/app")
	} else {
		args = append(args, s.opts.image, "/app/app")
	}
	log, err := os.Create(logName)
	if err != nil {
		return p, err
	}
	start := time.Now()
	end, timedOut, err := runContainer(name, s.label, args, nil, log, loadLimit)
	p.Seconds = int(time.Since(start).Seconds())
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
