package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReferenceSet(t *testing.T) {
	dists := []dist{
		{Dist: "B", Release: "B-1", DependentsTotal: 3, OnPerl: &perlReports{LinuxPass: 2, LinuxFail: 1}},
		{Dist: "A", Release: "A-2", OnPerl: &perlReports{LinuxPass: 1}},
		// As many failing as passing reports on Linux: outside.
		{Dist: "C", OnPerl: &perlReports{Pass: 9, LinuxPass: 1, LinuxFail: 1}},
		// No report on that perl: outside.
		{Dist: "D", Testers: &testers{Pass: 10}},
	}
	want := []refDist{
		{Dist: "A", Release: "A-2", LinuxPass: 1},
		{Dist: "B", Release: "B-1", LinuxPass: 2, LinuxFail: 1, DependentsTotal: 3},
	}
	if got := referenceSet(dists); !reflect.DeepEqual(got, want) {
		t.Errorf("referenceSet = %+v, want %+v", got, want)
	}
}

func TestBucketOf(t *testing.T) {
	for _, tc := range []struct {
		subject, reference string
		want               bucket
	}{
		{outcomePass, outcomePass, bucketBothPass},
		{outcomeFail, outcomePass, bucketSubjectOnlyFails},
		{"", outcomePass, bucketSubjectOnlyFails},
		{outcomeBlocked, outcomeFail, bucketBothFail},
		{outcomePass, "", bucketReferenceOnlyFails},
	} {
		if got := bucketOf(tc.subject, tc.reference); got != tc.want {
			t.Errorf("bucketOf(%q, %q) = %s, want %s", tc.subject, tc.reference, got, tc.want)
		}
	}
}

// comparisonFixture is a reference set of six distributions, a subject run
// in which Root failed to build and blocked Mid, which blocked Top, and a
// reference run in which everything but Gone passed.
func comparisonFixture() ([]refDist, *runData, *runData) {
	set := []refDist{
		{Dist: "Both", DependentsTotal: 1},
		{Dist: "Gone", DependentsTotal: 1},
		{Dist: "Mid", DependentsTotal: 5},
		{Dist: "Own", DependentsTotal: 2},
		{Dist: "Root", DependentsTotal: 9},
		{Dist: "Top", DependentsTotal: 0},
	}
	subject := newRunData("sub",
		[]*result{
			{Dist: "Both", Outcome: outcomePass},
			{Dist: "Gone", Outcome: outcomePass},
			{Dist: "Mid", Outcome: outcomeBlocked, BlockedBy: []string{"Root"}},
			{Dist: "Own", Outcome: outcomeFail},
			{Dist: "Root", Outcome: outcomeBuildFailed},
			{Dist: "Top", Outcome: outcomeBlocked, BlockedBy: []string{"Mid"}},
		},
		[]failure{
			{Dist: "Mid", Outcome: outcomeBlocked, BlockedBy: []string{"Root"}, RootBlockers: []string{"Root"}, Cause: causeBlocked},
			{Dist: "Own", Outcome: outcomeFail, Cause: causeCrash, Signature: "panic: at t/a.t line N."},
			{Dist: "Root", Outcome: outcomeBuildFailed, Cause: causeCAPI, DependentsTotal: 9, Signature: "error"},
			{Dist: "Top", Outcome: outcomeBlocked, BlockedBy: []string{"Mid"}, RootBlockers: []string{"Root"}, Cause: causeBlocked},
		})
	reference := newRunData("ref",
		[]*result{
			{Dist: "Both", Outcome: outcomePass},
			{Dist: "Gone", Outcome: outcomeFail},
			{Dist: "Mid", Outcome: outcomePass},
			{Dist: "Own", Outcome: outcomePass},
			{Dist: "Root", Outcome: outcomePass},
		}, nil)
	return set, subject, reference
}

func TestCompareRuns(t *testing.T) {
	set, subject, reference := comparisonFixture()
	c := compareRuns(set, subject, reference)
	// Top has no record in the reference run, which counts as not
	// passing there: both fail.
	wantBuckets := map[bucket]int{bucketBothPass: 1, bucketSubjectOnlyFails: 3, bucketBothFail: 1, bucketReferenceOnlyFails: 1}
	if !reflect.DeepEqual(c.Buckets, wantBuckets) {
		t.Errorf("buckets %v, want %v", c.Buckets, wantBuckets)
	}
	if c.SubjectPasses != 2 || c.ReferencePasses != 4 || c.ShareOfReferencePasses != 0.5/2 {
		t.Errorf("passes %d / %d, share %v; want 2 / 4, 0.25", c.SubjectPasses, c.ReferencePasses, c.ShareOfReferencePasses)
	}
	if c.RecordedByBoth != 5 {
		t.Errorf("recorded by both %d, want 5", c.RecordedByBoth)
	}
	wantOutcomes := map[string]int{outcomeBlocked: 1, outcomeFail: 1, outcomeBuildFailed: 1}
	if !reflect.DeepEqual(c.SubjectOnlyOutcomes, wantOutcomes) {
		t.Errorf("subject-only outcomes %v, want %v", c.SubjectOnlyOutcomes, wantOutcomes)
	}
	wantRoots := []rootBlocker{{Dist: "Root", Blocked: 1, SubjectOutcome: outcomeBuildFailed, ReferenceOutcome: outcomePass, Cause: causeCAPI, DependentsTotal: 9}}
	if !reflect.DeepEqual(c.RootBlockers, wantRoots) {
		t.Errorf("root blockers %+v, want %+v", c.RootBlockers, wantRoots)
	}
	wantBlocked := []distGroup{{Blocker: "Root", Dists: []string{"Mid"}}}
	if !reflect.DeepEqual(c.BlockedGroups, wantBlocked) {
		t.Errorf("blocked groups %+v, want %+v", c.BlockedGroups, wantBlocked)
	}
	wantOwn := []distGroup{
		{Outcome: outcomeBuildFailed, Cause: causeCAPI, Signature: "error", Dists: []string{"Root"}},
		{Outcome: outcomeFail, Cause: causeCrash, Signature: "panic: at FILE", Dists: []string{"Own"}},
	}
	if !reflect.DeepEqual(c.OwnGroups, wantOwn) {
		t.Errorf("own groups %+v, want %+v", c.OwnGroups, wantOwn)
	}
}

func TestCauseCounts(t *testing.T) {
	set, subject, reference := comparisonFixture()
	got := causeCounts(set, subject, reference)
	want := []causeCount{
		{Cause: causeCAPI, Distributions: 1, ReferencePasses: 1},
		{Cause: causeCrash, Distributions: 1, ReferencePasses: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("causeCounts = %+v, want %+v", got, want)
	}
}

// writeDataDir makes a data directory with a graph of the given
// distributions (built with -perl 5.44.0) and the results of one run.
func writeDataDir(t *testing.T, dists []dist, run string, results []result, logs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := writeJSONLines(filepath.Join(dir, distsFile), dists); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{summaryFile: `{"perl":"5.44.0"}`})
	if err := os.MkdirAll(filepath.Join(dir, "runs", run), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONLines(filepath.Join(dir, "runs", run, "results.jsonl"), results); err != nil {
		t.Fatal(err)
	}
	work := map[string]string{}
	for name, body := range logs {
		work[filepath.Join("runs", run, "work", name)] = body
	}
	writeFiles(t, dir, work)
	return dir
}

func TestReport(t *testing.T) {
	dists := []dist{
		{Dist: "A", Release: "A-1", DependentsTotal: 2, OnPerl: &perlReports{LinuxPass: 1}},
		{Dist: "B", Release: "B-1", DependentsTotal: 1, OnPerl: &perlReports{LinuxPass: 1}},
		{Dist: "C", Release: "C-1", OnPerl: &perlReports{LinuxPass: 1}},
		{Dist: "Lib", Release: "Lib-1", DependentsTotal: 3},
	}
	subject := writeDataDir(t, dists, "sub", []result{
		{Dist: "Lib", Outcome: outcomeBuildFailed, Phases: []phaseRecord{{Phase: "build", Status: 2}}},
		{Dist: "A", Outcome: outcomeFail, Phases: []phaseRecord{{Phase: "test", Status: 1}}},
		{Dist: "B", Outcome: outcomeBlocked, BlockedBy: []string{"Lib"}},
		// Recorded again: the last record counts.
		{Dist: "A", Outcome: outcomePass},
		{Dist: "C", Outcome: outcomeFail, Phases: []phaseRecord{{Phase: "test", Status: 1}}},
	}, map[string]string{
		"Lib/build.log": "Lib.xs:1:10: fatal error: zlib.h: No such file or directory\n",
		"C/test.log":    "t/a.t .. \nCan't locate Missing/Module.pm in @INC (you may need to install it)\n",
	})
	reference := writeDataDir(t, dists, "ref", []result{
		{Dist: "A", Outcome: outcomePass}, {Dist: "B", Outcome: outcomePass},
		{Dist: "C", Outcome: outcomePass}, {Dist: "Lib", Outcome: outcomePass},
	}, nil)
	var out bytes.Buffer
	if err := report(subject, "sub", interpreterGoPerl, referenceRun{dataDir: reference, run: "ref"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"the reference): 3 distributions, 3 recorded, 1 pass",
		"sub only fails                    2 (66.7%)",
		"ref passes 3, sub passes 1; sub passes 33.3% of what ref passes",
		"      1 Lib ",
		"a module not found at run time",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report output lacks %q:\n%s", want, out.String())
		}
	}
	runDir := filepath.Join(subject, "runs", "sub")
	raw, err := os.ReadFile(filepath.Join(runDir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s runSummary
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Comparison == nil || s.Comparison.Buckets[bucketSubjectOnlyFails] != 2 || len(s.Comparison.RootBlockers) != 1 {
		t.Errorf("summary comparison %+v", s.Comparison)
	}
	failures, err := readJSONLines[failure](filepath.Join(runDir, "failures.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	causes := map[string]cause{}
	for _, f := range failures {
		causes[f.Dist] = f.Cause
	}
	want := map[string]cause{"B": causeBlocked, "C": causeModuleNotFound, "Lib": causeExternalLibrary}
	if !reflect.DeepEqual(causes, want) {
		t.Errorf("causes %v, want %v", causes, want)
	}
	md, err := os.ReadFile(filepath.Join(runDir, "failures.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "| Lib | build_failed | an external C library, header or tool is missing | 1 | 3 |") {
		t.Errorf("failures.md lacks the blocker row:\n%s", md)
	}
}
