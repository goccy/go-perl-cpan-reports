package main

// The static build phase in the results directory and on the site:
//
//	packages.jsonl.gz          every distribution of the snapshot: release,
//	                           its modules with their versions in the package
//	                           index, and the distributions its configure,
//	                           build and runtime requirements name (refset)
//	<run>/provided.json        the modules the interpreter ships (export)
//	<run>/static.jsonl.gz      the static build record of each distribution
//	<run>/static-logs.jsonl.gz the end of the log of each failure
//
// The cpanfile checker (check.html) reads check.json, which the site makes
// from them.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
)

const (
	packagesGzFile   = "packages.jsonl.gz"
	staticGzFile     = "static.jsonl.gz"
	staticLogsGzFile = "static-logs.jsonl.gz"
)

// staticMeta is the "static" part of a run's meta.json: what the static
// build phase measured with, and how.
type staticMeta struct {
	Gperl gperlBuild `json:"gperl"`
	// Go, CC and LibC are the versions of the toolchain the binaries were
	// built with.
	Go          string `json:"go"`
	CC          string `json:"cc"`
	LibC        string `json:"libc"`
	BuildFlags  string `json:"build_flags"`
	Environment string `json:"environment"`
	// CRuntime are the libraries that do not count as external.
	CRuntime    []string           `json:"c_runtime"`
	FirstResult string             `json:"first_result"`
	LastResult  string             `json:"last_result"`
	Recorded    int                `json:"recorded"`
	Invocations []staticInvocation `json:"invocations"`
}

// packageDist is one line of packages.jsonl.gz.
type packageDist struct {
	Dist    string `json:"dist"`
	Release string `json:"release"`
	// Modules are the modules the package index assigns to the release,
	// each with its version there ("undef" for none).
	Modules [][2]string `json:"modules,omitempty"`
	// Needs are the distributions providing the configure, build and
	// runtime requirements of its metadata that the interpreter does not
	// meet itself.
	Needs []string `json:"needs,omitempty"`
}

// packageIndexOf makes packages.jsonl.gz of a data directory's graph.
func packageIndexOf(dataDir string, dists []dist) ([]packageDist, error) {
	index, err := readPackageIndex(filepath.Join(dataDir, packagesFile))
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(dists))
	for _, d := range dists {
		known[d.Dist] = true
	}
	out := make([]packageDist, 0, len(dists))
	for _, d := range dists {
		p := packageDist{Dist: d.Dist, Release: d.Release}
		for _, m := range d.Modules {
			v := index[m].version
			if v == "undef" {
				v = ""
			}
			p.Modules = append(p.Modules, [2]string{m, v})
		}
		for _, r := range d.Requires {
			if r.Relationship != "requires" || !staticPhases[r.Phase] || r.Provided || !known[r.Dist] || r.Dist == d.Dist {
				continue
			}
			if !slices.Contains(p.Needs, r.Dist) {
				p.Needs = append(p.Needs, r.Dist)
			}
		}
		sort.Strings(p.Needs)
		out = append(out, p)
	}
	return out, nil
}

// exportStatic writes the run's static build records and the ends of its
// failing logs into the run's directory of the results, and returns the
// "static" part of its meta.json; nil, and no files, when the run has no
// static build phase.
func exportStatic(dataDir, run, dir string, red *redactor) (*staticMeta, error) {
	runDir := filepath.Join(dataDir, "runs", run)
	rows, err := readJSONLines[staticRecord](filepath.Join(runDir, "static.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		for _, name := range []string{staticGzFile, staticLogsGzFile} {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		return nil, nil
	}
	latest := latestStatic(rows)
	m := &staticMeta{
		BuildFlags: staticBuildFlags, Environment: staticEnvironment, Recorded: len(latest),
	}
	for lib := range cRuntimeLibraries {
		m.CRuntime = append(m.CRuntime, lib)
	}
	sort.Strings(m.CRuntime)
	probe := filepath.Join(runDir, "static", "probe")
	raw, err := os.ReadFile(filepath.Join(probe, "gperl.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &m.Gperl); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(probe, "gperl.json"), err)
	}
	raw, err = os.ReadFile(filepath.Join(probe, "versions.json"))
	if err != nil {
		return nil, err
	}
	var versions struct{ Go, CC, LibC string }
	if err := json.Unmarshal(raw, &versions); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(probe, "versions.json"), err)
	}
	m.Go, m.CC, m.LibC = versions.Go, versions.CC, versions.LibC
	if m.Invocations, err = readJSONLines[staticInvocation](filepath.Join(runDir, "static-invocations.jsonl")); err != nil {
		return nil, err
	}
	var logs []logExcerpt
	for _, rec := range latest {
		if m.FirstResult == "" || rec.Finished < m.FirstResult {
			m.FirstResult = rec.Finished
		}
		if rec.Finished > m.LastResult {
			m.LastResult = rec.Finished
		}
		name := staticLogOf(rec)
		if name == "" {
			continue
		}
		text, size, err := logTailText(filepath.Join(runDir, "static", "dists", rec.Dist, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if size > 0 {
			logs = append(logs, logExcerpt{Dist: rec.Dist, Log: name, Bytes: size, Text: red.text(text)})
		}
	}
	if err := writeGzipJSONLines(filepath.Join(dir, staticGzFile), latest); err != nil {
		return nil, err
	}
	if err := writeGzipJSONLines(filepath.Join(dir, staticLogsGzFile), logs); err != nil {
		return nil, err
	}
	return m, nil
}

// latestStatic returns the last record of each distribution, sorted by name.
func latestStatic(rows []staticRecord) []*staticRecord {
	latest := map[string]*staticRecord{}
	for i := range rows {
		latest[rows[i].Dist] = &rows[i]
	}
	out := make([]*staticRecord, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dist < out[j].Dist })
	return out
}

// staticLogOf names the log that tells why a distribution is not static:
// the one of the phase its reason is about; for a static one, the one that
// tells why its tests did not pass with its test binary; "" when no log
// tells more than the record.
func staticLogOf(rec *staticRecord) string {
	if rec.State == staticDriverFailed {
		return "container.log"
	}
	if rec.State == staticStatic && rec.Test != nil {
		switch rec.Test.State {
		case testDepsFailed:
			return "test_modules.log"
		case testLinkFailed:
			return "test_link.log"
		case testNeedsEnvironment, testStaticFailed, testDiffersFromMakeTest, testTimeout:
			return "test_empty.log"
		}
	}
	if rec.State != staticNotStatic && rec.State != staticDependency {
		return ""
	}
	switch rec.Reason {
	case reasonDistBuildFailed, reasonLinkerOverridden, reasonModulesTimeout:
		return "modules.log"
	case reasonSharedLibraryPathDropped, reasonNeedsSharedLibrary, reasonCrossModuleSymbols,
		reasonCRuntimeNotLinked, reasonLinkFailed, reasonLinkTimeout:
		return "link.log"
	case reasonNeedsEnvironment, reasonLoadFailed, reasonLoadTimeout:
		return "run_empty.log"
	}
	return ""
}

// readPublishedStatic reads a run's static build records and log ends from
// its directory of the results; none when it has no static build phase.
func readPublishedStatic(dir string) (map[string]*staticRecord, map[string]*logExcerpt, error) {
	records := map[string]*staticRecord{}
	logs := map[string]*logExcerpt{}
	rows, err := readGzipJSONLines[staticRecord](filepath.Join(dir, staticGzFile))
	if errors.Is(err, os.ErrNotExist) {
		return records, logs, nil
	}
	if err != nil {
		return nil, nil, err
	}
	known := map[string]bool{}
	for _, c := range staticCategories() {
		known[c.ID] = true
	}
	for _, c := range staticTestCategories() {
		known[c.ID] = true
	}
	for i := range rows {
		r := &rows[i]
		if id := staticCategoryOf(r); id != "" && !known[id] || id == "" && r.State != staticDependency {
			return nil, nil, fmt.Errorf("%s: %s: unknown state %q, reason %q", staticGzFile, r.Dist, r.State, r.Reason)
		}
		if id := staticTestCategoryOf(r); id != "" && !known[id] {
			return nil, nil, fmt.Errorf("%s: %s: unknown test state %q", staticGzFile, r.Dist, r.Test.State)
		}
		records[r.Dist] = r
	}
	excerpts, err := readGzipJSONLines[logExcerpt](filepath.Join(dir, staticLogsGzFile))
	if err != nil {
		return nil, nil, err
	}
	for i := range excerpts {
		logs[excerpts[i].Dist] = &excerpts[i]
	}
	return records, logs, nil
}

// staticReasonTitles says what each reason means, as the site shows it.
var staticReasonTitles = map[string]string{
	reasonBundlesSystemLibraries:   "links libraries of the system from their static archives",
	reasonArchiveNotSupported:      "gperl takes no archive of its name (gperl takes Name-Version.tar.gz, .tgz, .tar.bz2 and .zip)",
	reasonDistBuildFailed:          "its build failed in gperl's module directory",
	reasonLinkerOverridden:         "its build links its native module itself, which gperl cannot link statically",
	reasonModulesTimeout:           "building it, or what it needs, in gperl's module directory ran out of time",
	reasonLinkTimeout:              "the static link ran out of time",
	reasonSharedLibraryPathDropped: "a native module names a shared library by its path, which gperl does not pass to the static link",
	reasonNeedsSharedLibrary:       "a native module links a library that has no static archive",
	reasonCrossModuleSymbols:       "a native module uses symbols of another native module, which a static link of gperl's modules does not resolve",
	reasonCRuntimeNotLinked:        "a native module uses functions of libm or libcrypt that its link did not name",
	reasonLinkFailed:               "the static link failed",
	reasonNotFullyStatic:           "the binary names a program interpreter or shared libraries",
	reasonNeedsEnvironment:         "the binary failed where there is nothing but the binary, and ran in the build environment",
	reasonLoadFailed:               "the binary failed to load the modules, in the empty container and in the build environment",
	reasonLoadTimeout:              "the binary ran out of time in the empty container",
}

// staticCategory is one way the site groups a record (D2): self-contained,
// bundling libraries of the system, or not static for a reason.
type staticCategory struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// staticCategories lists the groups in the order they are presented.
func staticCategories() []staticCategory {
	out := []staticCategory{
		{"static", "Static (self-contained)"},
		{"static:" + reasonBundlesSystemLibraries, "Static (links system libraries statically)"},
	}
	for _, r := range []string{
		reasonNeedsSharedLibrary, reasonSharedLibraryPathDropped, reasonCrossModuleSymbols, reasonCRuntimeNotLinked,
		reasonLinkFailed, reasonLinkTimeout, reasonNotFullyStatic, reasonNeedsEnvironment, reasonLoadFailed, reasonLoadTimeout,
		reasonDistBuildFailed, reasonLinkerOverridden, reasonModulesTimeout, reasonArchiveNotSupported,
	} {
		out = append(out, staticCategory{"not_static:" + r, "Not static: " + staticReasonTitles[r]})
	}
	return append(out,
		staticCategory{"blocked", "Not static: a distribution it needs did not build"},
		staticCategory{"driver_failed", "Not measured: the harness could not record a result"},
	)
}

// staticCategoryOf returns the ID of the group of a record, "" for one that
// was not measured (a dependency, or no record).
func staticCategoryOf(rec *staticRecord) string {
	if rec == nil {
		return ""
	}
	switch rec.State {
	case staticStatic:
		if rec.Reason != "" {
			return "static:" + rec.Reason
		}
		return "static"
	case staticNotStatic:
		return "not_static:" + rec.Reason
	case staticBlocked, staticDriverFailed:
		return rec.State
	}
	return ""
}

// staticTestCategories lists how a static target's own tests went with its
// test binary, in the order they are presented.
func staticTestCategories() []staticCategory {
	return []staticCategory{
		{"test:" + testPass, "Tests pass with the binary alone"},
		{"test:" + testNeedsEnvironment, "Tests fail with the binary alone, and pass with it in the build environment"},
		{"test:" + testStaticFailed, "Tests fail with the binary, and pass with gperl test"},
		{"test:" + testDiffersFromMakeTest, "Tests fail with the binary and with gperl test (they passed under make test)"},
		{"test:" + testTimeout, "Tests ran out of time with the binary alone"},
		{"test:" + testDepsFailed, "Tests not run: a distribution only they need did not build"},
		{"test:" + testLinkFailed, "Tests not run: gperl test -c did not build the test binary"},
		{"test:" + testNoRecord, "Tests not run: the run recorded no test files"},
	}
}

// staticTestCategoryOf returns the ID of the test group of a record, "" when
// its tests did not run with a test binary.
func staticTestCategoryOf(rec *staticRecord) string {
	if rec == nil || rec.Test == nil {
		return ""
	}
	return "test:" + rec.Test.State
}

// staticView is what a distribution's page shows of its static build.
type staticView struct {
	Rec      *staticRecord
	Category staticCategory
	// Test is the group of how its tests went with its test binary; nil
	// when they did not run.
	Test *staticCategory
	Log  *logExcerpt
	// Libraries are the external libraries' names, once each.
	Libraries []string
}

// staticViewOf returns the static build of a distribution in run r, nil
// when the run has no static build phase or did not measure it.
func staticViewOf(r *siteRun, name string) *staticView {
	rec := r.static[name]
	id := staticCategoryOf(rec)
	if id == "" {
		return nil
	}
	v := &staticView{Rec: rec, Log: r.staticLogs[name]}
	for _, c := range staticCategories() {
		if c.ID == id {
			v.Category = c
		}
	}
	for _, c := range staticTestCategories() {
		if c.ID == staticTestCategoryOf(rec) {
			v.Test = &c
		}
	}
	for _, l := range rec.ExternalLibs {
		if !slices.Contains(v.Libraries, l.Name) {
			v.Libraries = append(v.Libraries, l.Name)
		}
	}
	return v
}

// staticCount is one row of the overview's table of the static build.
type staticCount struct {
	Category staticCategory
	Count    int
}

// staticCounts counts the measured distributions of the reference set by
// group, in the order of staticCategories, leaving out empty groups.
func staticCounts(r *siteRun, inSet map[string]*refDist) []staticCount {
	return countStatic(r, inSet, staticCategoryOf, staticCategories())
}

// staticTestCounts counts the distributions of the reference set whose tests
// ran with a test binary by how they went, in the order of
// staticTestCategories, leaving out empty groups.
func staticTestCounts(r *siteRun, inSet map[string]*refDist) []staticCount {
	return countStatic(r, inSet, staticTestCategoryOf, staticTestCategories())
}

// countStatic counts the records of the reference set by the group of
// groups that of gives them.
func countStatic(r *siteRun, inSet map[string]*refDist, of func(*staticRecord) string, groups []staticCategory) []staticCount {
	n := map[string]int{}
	for name, rec := range r.static {
		if id := of(rec); id != "" && inSet[name] != nil {
			n[id]++
		}
	}
	var out []staticCount
	for _, c := range groups {
		if n[c.ID] > 0 {
			out = append(out, staticCount{c, n[c.ID]})
		}
	}
	return out
}

// checkData is check.json, what the cpanfile checker reads: every
// distribution of the snapshot (dists, by index), every module of the
// package index (modules) and the modules the interpreter ships (stdlib).
//
// A row of dists is [name, release, outcome, static, libraries, page,
// needs, closure]: outcome indexes Outcomes (-1: no record), static indexes
// Static (-1: not measured), libraries are the external libraries' names,
// page is the name of its page on the site ("" when it has none), needs
// index dists (its metadata's configure, build and runtime requirements),
// and closure, when its static build was measured, indexes the
// distributions its program carried. A row of modules is [name, dist
// index, version].
type checkData struct {
	Subject  string           `json:"subject"`
	Perl     string           `json:"perl"`
	Outcomes []string         `json:"outcomes"`
	Static   []staticCategory `json:"static"`
	Columns  []string         `json:"columns"`
	Dists    [][]any          `json:"dists"`
	Modules  [][]any          `json:"modules"`
	Stdlib   map[string]any   `json:"stdlib"`
}

// checkDataOf makes check.json from the snapshot's packages and the
// subject run.
func checkDataOf(s *siteData) (*checkData, error) {
	c := &checkData{
		Subject: s.Subject.Name, Perl: s.Snapshot.Perl, Outcomes: outcomes, Static: staticCategories(),
		Columns: []string{"dist", "release", "outcome", "static", "libraries", "page", "needs", "closure"},
		Stdlib:  map[string]any{},
	}
	for module, v := range s.Subject.provided {
		if v == "" {
			c.Stdlib[module] = nil
		} else {
			c.Stdlib[module] = v
		}
	}
	index := make(map[string]int, len(s.packages))
	for i, p := range s.packages {
		index[p.Dist] = i
	}
	outcomeIndex := map[string]int{}
	for i, o := range outcomes {
		outcomeIndex[o] = i
	}
	staticIndex := map[string]int{}
	for i, cat := range c.Static {
		staticIndex[cat.ID] = i
	}
	for _, p := range s.packages {
		outcome := -1
		if res := s.Subject.data.latest[p.Dist]; res != nil {
			i, ok := outcomeIndex[res.Outcome]
			if !ok {
				return nil, fmt.Errorf("check.json: %s: unknown outcome %q", p.Dist, res.Outcome)
			}
			outcome = i
		}
		static := -1
		var libs []string
		closure := []int{}
		if rec := s.Subject.static[p.Dist]; rec != nil {
			if id := staticCategoryOf(rec); id != "" {
				static = staticIndex[id]
				for _, l := range rec.ExternalLibs {
					if !slices.Contains(libs, l.Name) {
						libs = append(libs, l.Name)
					}
				}
				for _, d := range rec.Closure {
					if i, ok := index[d]; ok {
						closure = append(closure, i)
					}
				}
			}
		}
		page := s.pages[p.Dist]
		needs := []int{}
		for _, d := range p.Needs {
			if i, ok := index[d]; ok {
				needs = append(needs, i)
			}
		}
		if libs == nil {
			libs = []string{}
		}
		c.Dists = append(c.Dists, []any{p.Dist, p.Release, outcome, static, libs, page, needs, closure})
		for _, m := range p.Modules {
			c.Modules = append(c.Modules, []any{m[0], index[p.Dist], m[1]})
		}
	}
	sort.Slice(c.Modules, func(i, j int) bool { return c.Modules[i][0].(string) < c.Modules[j][0].(string) })
	return c, nil
}
