package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKnownCausesFile(t *testing.T) {
	if _, err := knownCauses(); err != nil {
		t.Fatal(err)
	}
	listed := map[cause]bool{}
	for _, c := range causes {
		listed[c] = true
	}
	for _, c := range knownCauseKinds {
		if !listed[c] || causeTitles[c] == "" {
			t.Errorf("cause %s is not listed in causes with a title", c)
		}
	}
}

func TestParseKnownCausesRefuses(t *testing.T) {
	for name, raw := range map[string]string{
		"a cause the logs tell": `[{"dist":"A","cause":"crash","tests":["t/a.t"],"evidence":"e"}]`,
		"no test files":         `[{"dist":"A","cause":"fork-shares-native-memory","tests":[],"evidence":"e"}]`,
		"no evidence":           `[{"dist":"A","cause":"fork-shares-native-memory","tests":["t/a.t"]}]`,
		"no distribution":       `[{"cause":"fork-shares-native-memory","tests":["t/a.t"],"evidence":"e"}]`,
		"a file listed twice":   `[{"dist":"A","cause":"fork-shares-native-memory","tests":["t/a.t","t/a.t"],"evidence":"e"}]`,
		"an unknown field":      `[{"dist":"A","cause":"fork-shares-native-memory","tests":["t/a.t"],"evidence":"e","file":"t/a.t"}]`,
		"two entries of one distribution": `[{"dist":"A","cause":"fork-shares-native-memory","tests":["t/a.t"],"evidence":"e"},
			{"dist":"A","cause":"fork-shares-native-memory","tests":["t/b.t"],"evidence":"e"}]`,
	} {
		if _, err := parseKnownCauses([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// failingTest is a test file the harness counted as failing.
func failingTest(file string) testFile {
	return testFile{File: file, Planned: 2, Run: 2, Passed: 1, Failed: 1, Exit: 1}
}

func TestApplyKnownCauses(t *testing.T) {
	known, err := parseKnownCauses([]byte(`[
		{"dist":"Covered","cause":"fork-shares-native-memory","tests":["t/fork.t","t/server.t","t/other-run.t"],"evidence":"e"},
		{"dist":"AlsoOther","cause":"fork-shares-native-memory","tests":["t/fork.t"],"evidence":"e"},
		{"dist":"Blocked","cause":"fork-shares-native-memory","tests":["t/fork.t"],"evidence":"e"},
		{"dist":"TimedOut","cause":"fork-shares-native-memory","tests":["t/fork.t"],"evidence":"e"},
		{"dist":"NoFailingFile","cause":"fork-shares-native-memory","tests":["t/fork.t"],"evidence":"e"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	run := func() *runData {
		passing := testFile{File: "t/basic.t", Planned: 1, Run: 1, Passed: 1}
		return newRunData("r",
			[]*result{
				// Every failing file is among the entry's (not all of the
				// entry's files need to fail).
				{Dist: "Covered", Outcome: outcomeFail, Tests: []testFile{passing, failingTest("t/fork.t"), failingTest("t/server.t")}},
				// Another file failed too.
				{Dist: "AlsoOther", Outcome: outcomeFail, Tests: []testFile{failingTest("t/fork.t"), failingTest("t/other.t")}},
				// What it lacked is what it is put down to.
				{Dist: "Blocked", Outcome: outcomeBlocked, OwnOutcome: outcomeFail, Tests: []testFile{failingTest("t/fork.t")}},
				{Dist: "TimedOut", Outcome: outcomeTimeout, Tests: []testFile{failingTest("t/fork.t")}},
				// Tests that failed in no file the harness reported.
				{Dist: "NoFailingFile", Outcome: outcomeFail, Tests: []testFile{passing}},
				{Dist: "Unlisted", Outcome: outcomeFail, Tests: []testFile{failingTest("t/fork.t")}},
			},
			[]failure{
				{Dist: "Covered", Cause: causeCrash},
				{Dist: "AlsoOther", Cause: causeCrash},
				{Dist: "Blocked", Cause: causeBlocked},
				{Dist: "TimedOut", Cause: causeTimeLimitBlocked},
				{Dist: "NoFailingFile", Cause: causeNotAnalysed},
				{Dist: "Unlisted", Cause: causeCrash},
			})
	}
	causesOf := func(r *runData) map[string]cause {
		out := map[string]cause{}
		for name, f := range r.failures {
			out[name] = f.Cause
		}
		return out
	}

	goPerl := run()
	goPerl.applyKnownCauses(known, interpreterGoPerl)
	want := map[string]cause{
		"Covered": causeForkSharesNativeMemory, "AlsoOther": causeCrash, "Blocked": causeBlocked,
		"TimedOut": causeTimeLimitBlocked, "NoFailingFile": causeNotAnalysed, "Unlisted": causeCrash,
	}
	for name, c := range causesOf(goPerl) {
		if c != want[name] {
			t.Errorf("go-perl run: %s has cause %s, want %s", name, c, want[name])
		}
	}

	// A run of perl itself keeps every cause its evidence tells, the
	// covered distribution's too.
	perl := run()
	perl.applyKnownCauses(known, interpreterPerl)
	if c := perl.failures["Covered"].Cause; c != causeCrash {
		t.Errorf("perl run: Covered has cause %s, want %s", c, causeCrash)
	}
}

// withKnownCauses makes the command use raw as its known-causes file for
// the rest of the test.
func withKnownCauses(t *testing.T, raw string) {
	t.Helper()
	old := knownCausesFile
	knownCausesFile = []byte(raw)
	t.Cleanup(func() { knownCausesFile = old })
}

// TestKnownCausesThroughExportAndSite exports a go-perl run and the perl run
// it is compared with, in both of which Forky failed in the test file its
// entry lists and Mixed in a file its entry lists and another, and
// generates the site.
func TestKnownCausesThroughExportAndSite(t *testing.T) {
	withKnownCauses(t, `[
		{"dist":"Forky","cause":"fork-shares-native-memory","tests":["t/fork.t"],
		 "evidence":"The <handle> is shared with the child: a fault on the fork child."},
		{"dist":"Mixed","cause":"fork-shares-native-memory","tests":["t/fork.t"],"evidence":"e"}
	]`)
	dists := []dist{
		{Dist: "Forky", Release: "Forky-1", OnPerl: &perlReports{LinuxPass: 1}},
		{Dist: "Mixed", Release: "Mixed-1", OnPerl: &perlReports{LinuxPass: 1}},
		{Dist: "Good", Release: "Good-1", OnPerl: &perlReports{LinuxPass: 1}},
	}
	results := []result{
		{Dist: "Forky", Outcome: outcomeFail, Phases: []phaseRecord{{Phase: "test", Status: 1}},
			Tests: []testFile{failingTest("t/fork.t")}, Finished: "2026-01-02T00:00:00Z"},
		{Dist: "Mixed", Outcome: outcomeFail, Phases: []phaseRecord{{Phase: "test", Status: 1}},
			Tests: []testFile{failingTest("t/fork.t"), failingTest("t/other.t")}, Finished: "2026-01-02T00:00:00Z"},
		{Dist: "Good", Outcome: outcomePass, Finished: "2026-01-02T00:00:00Z"},
	}
	logs := map[string]string{
		"Forky/test.log": "t/fork.t .. \nSIGSEGV: segmentation violation\n",
		"Mixed/test.log": "t/fork.t .. \nSIGSEGV: segmentation violation\nt/other.t .. \nsomething else\n",
	}
	snapshot := `{"fetched_at":"2026-01-01T00:00:00Z","packages_url":"u","packages_sha256":"abc","releases":3}`
	inv := `{"image":"i","harness":{"module":"m"},"jobs":1,"cpus":"1","memory":"1g","wall_seconds":60,"test_seconds":30}` + "\n"
	subject := writeDataDir(t, dists, "sub", results, logs)
	reference := writeDataDir(t, dists, "ref", results, logs)
	writeFiles(t, subject, map[string]string{snapshotFile: snapshot, "runs/sub/invocations.jsonl": inv})
	writeFiles(t, reference, map[string]string{snapshotFile: snapshot, "runs/ref/invocations.jsonl": inv})
	out := filepath.Join(t.TempDir(), "results")
	if err := exportReferenceSet(reference, out); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []exportOptions{
		{dataDir: subject, run: "sub", results: out, interpreter: interpreterMeta{Name: interpreterGoPerl, Commit: "abc"}, comparedWith: "ref"},
		{dataDir: reference, run: "ref", results: out, interpreter: interpreterMeta{Name: interpreterPerl, Version: "5.44.0"}},
	} {
		if err := exportRun(opts); err != nil {
			t.Fatal(err)
		}
	}

	// export writes the cause into the go-perl run's records only.
	exported := func(run string) map[string]cause {
		t.Helper()
		rows, err := readGzipJSONLines[failure](filepath.Join(out, run, failuresGzFile))
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]cause{}
		for _, f := range rows {
			m[f.Dist] = f.Cause
		}
		return m
	}
	if got := exported("sub"); got["Forky"] != causeForkSharesNativeMemory || got["Mixed"] != causeCrash {
		t.Errorf("go-perl run's exported causes: %v", got)
	}
	if got := exported("ref"); got["Forky"] != causeCrash || got["Mixed"] != causeCrash {
		t.Errorf("perl run's exported causes: %v", got)
	}

	site := filepath.Join(t.TempDir(), "site")
	if err := generateSite(out, site); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(site, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	index := read("index.html")
	for _, want := range []string{
		`<section id="known-limitations">`,
		"<h2>Known limitations</h2>",
		"one virtual address can only refer\nto one memory at a time",
		"In run <code>sub</code>, 1 distribution failed for\nthis reason, 1 of them in the reference set:",
		// The list of the distributions: Forky alone.
		"<p><a href=\"d/Forky.html\">Forky</a></p>",
	} {
		if !strings.Contains(index, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	forky := read("d/Forky.html")
	if !strings.Contains(forky, "The &lt;handle&gt; is shared with the child: a fault on the fork child.") ||
		strings.Count(forky, causeTitles[causeForkSharesNativeMemory]) != 1 {
		t.Errorf("d/Forky.html: the analysis is not shown once, for the go-perl run alone:\n%s", forky)
	}
	if strings.Contains(read("d/Mixed.html"), "known-limitations") {
		t.Errorf("d/Mixed.html shows the known limitation")
	}
}
