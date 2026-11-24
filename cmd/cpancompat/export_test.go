package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTailLines(t *testing.T) {
	for _, tc := range []struct {
		text     string
		n, limit int
		want     string
	}{
		{"a\nb\nc\n", 2, 100, "b\nc"},
		{"a\nb\nc", 5, 100, "a\nb\nc"},
		// The byte limit ends it before the line limit.
		{"aaaa\nbb\ncc\n", 5, 5, "bb\ncc"},
		// A last line longer than the limit: its end.
		{"x\n0123456789", 5, 4, "6789"},
		// Cut at a character boundary.
		{"ééé", 1, 3, "é"},
		{"", 3, 10, ""},
	} {
		if got := tailLines(tc.text, tc.n, tc.limit); got != tc.want {
			t.Errorf("tailLines(%q, %d, %d) = %q, want %q", tc.text, tc.n, tc.limit, got, tc.want)
		}
	}
}

func TestLogTailTextReadsTheEnd(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for b.Len() < 3*excerptWindow {
		b.WriteString("an early line that is not in the excerpt\n")
	}
	for i := 0; i < excerptLines+5; i++ {
		b.WriteString("late\n")
	}
	writeFiles(t, dir, map[string]string{"test.log": b.String()})
	text, size, err := logTailText(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(b.Len()) {
		t.Errorf("size %d, want %d", size, b.Len())
	}
	if want := strings.TrimSuffix(strings.Repeat("late\n", excerptLines), "\n"); text != want {
		t.Errorf("excerpt has %d lines, want %d of late", strings.Count(text, "\n")+1, excerptLines)
	}
}

func TestExcerptOf(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"build.log": "cc -c x.c\nx.c:1: error: no\n", "container.log": "docker said\n", "test.log": ""})
	ex, ok := excerptOf(dir, &failure{Dist: "X", Phase: "build"})
	if !ok || ex.Log != "build.log" || ex.Text != "cc -c x.c\nx.c:1: error: no" || ex.Bytes != 27 {
		t.Errorf("build failure: %+v, %v", ex, ok)
	}
	// No failing phase: what docker printed, the test log being empty.
	if ex, ok := excerptOf(dir, &failure{Dist: "X", Outcome: outcomeDriverFailed}); !ok || ex.Log != "container.log" {
		t.Errorf("driver failure: %+v, %v", ex, ok)
	}
	// Blocked and never run: nothing.
	if _, ok := excerptOf(dir, &failure{Dist: "X", Outcome: outcomeBlocked}); ok {
		t.Errorf("a blocked distribution that did not run has an excerpt")
	}
}

func TestRedactor(t *testing.T) {
	data := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	r, err := newRedactor(data)
	if err != nil {
		t.Fatal(err)
	}
	f := failure{
		Evidence: []string{
			"cannot open " + data + "/archives/x.tar.gz",
			"\t" + home + "/src/go-perl/internal/perl.go:668 +0x19c",
			"cc is " + home + " and --sysroot=" + home + "/.config/sdk/share/sysroot)",
		},
		Signature: data + "/runs",
		Stuck:     &stuck{Processes: []stuckProcess{{Args: []string{home + "/bin/x"}}}},
	}
	r.failure(&f)
	want := failure{
		Evidence: []string{
			"cannot open <data>/archives/x.tar.gz",
			"\t<home>/…/internal/perl.go:668 +0x19c",
			"cc is <home> and --sysroot=<home>/…/share/sysroot)",
		},
		Signature: "<data>/runs",
		Stuck:     &stuck{Processes: []stuckProcess{{Args: []string{"<home>/…/bin/x"}}}},
	}
	// The data directory may lie below the home directory: it is replaced
	// first.
	if !reflect.DeepEqual(f, want) {
		t.Errorf("redacted %+v, want %+v", f, want)
	}
}

func TestExportAndReadPublished(t *testing.T) {
	dists := []dist{
		{Dist: "A", Release: "A-1", DependentsTotal: 1, OnPerl: &perlReports{LinuxPass: 2}},
		{Dist: "B", Release: "B-1", OnPerl: &perlReports{LinuxPass: 1}},
		{Dist: "Lib", Release: "Lib-1", DependentsTotal: 2},
	}
	subject := writeDataDir(t, dists, "sub", []result{
		{Dist: "Lib", Outcome: outcomeBuildFailed, Phases: []phaseRecord{{Phase: "build", Status: 2}}, Finished: "2026-01-01T00:00:02Z"},
		{Dist: "A", Outcome: outcomePass, Finished: "2026-01-01T00:00:01Z"},
		{Dist: "B", Outcome: outcomeBlocked, BlockedBy: []string{"Lib"}, Finished: "2026-01-01T00:00:03Z"},
	}, map[string]string{"Lib/build.log": "Lib.xs:1:10: fatal error: zlib.h: No such file or directory\n"})
	writeFiles(t, subject, map[string]string{
		snapshotFile:                 `{"fetched_at":"2026-01-01T00:00:00Z","packages_url":"u","packages_sha256":"abc","releases":3}`,
		"runs/sub/invocations.jsonl": `{"started":"2026-01-01T00:00:00Z","image":"cpancompat:x","harness":{"module":"m"},"jobs":2,"cpus":"1","memory":"1g","wall_seconds":7200,"test_seconds":300}` + "\n",
	})
	results := filepath.Join(t.TempDir(), "results")
	opts := exportOptions{
		dataDir: subject, run: "sub", results: results,
		interpreter: interpreterMeta{Name: "go-perl", Commit: "c0ffee"}, provisional: true, note: "a trial",
	}
	if err := exportRun(opts); err == nil {
		t.Errorf("export without a reference set succeeded")
	}
	if err := exportReferenceSet(subject, results); err != nil {
		t.Fatal(err)
	}
	if err := exportRun(opts); err != nil {
		t.Fatal(err)
	}
	p, err := readPublished(results)
	if err != nil {
		t.Fatal(err)
	}
	if p.snapshot.ReferenceDistributions != 2 || p.snapshot.Perl != "5.44.0" || len(p.set) != 2 {
		t.Errorf("snapshot %+v, set %+v", p.snapshot, p.set)
	}
	pr := p.runs["sub"]
	if pr == nil {
		t.Fatalf("runs %v", p.runs)
	}
	m := pr.meta
	if m.Interpreter.Name != "go-perl" || !m.Provisional || m.FirstResult != "2026-01-01T00:00:01Z" ||
		m.LastResult != "2026-01-01T00:00:03Z" || m.Recorded != 3 || len(m.Invocations) != 1 || m.Invocations[0].TestSeconds != 300 {
		t.Errorf("meta %+v", m)
	}
	if pr.data.outcome("B") != outcomeBlocked || pr.data.failures["Lib"].Cause != causeExternalLibrary {
		t.Errorf("records: B %s, Lib %+v", pr.data.outcome("B"), pr.data.failures["Lib"])
	}
	if ex := pr.logs["Lib"]; ex == nil || ex.Log != "build.log" {
		t.Errorf("logs %+v", pr.logs)
	}

	// The same records make the same files.
	before, err := os.ReadFile(filepath.Join(results, "sub", failuresGzFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := exportRun(opts); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(results, "sub", failuresGzFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("exporting again changed %s", failuresGzFile)
	}

	// A run of another snapshot is not exported into the results; a new
	// snapshot replaces the one the runs there were measured on, and they
	// stay, joined with it by distribution name.
	writeFiles(t, subject, map[string]string{snapshotFile: `{"packages_sha256":"other"}`})
	if err := exportRun(opts); err == nil {
		t.Errorf("a run of another snapshot was exported")
	}
	if err := exportReferenceSet(subject, results); err != nil {
		t.Fatalf("replacing the snapshot of results with runs: %v", err)
	}
	if p, err := readPublished(results); err != nil || p.snapshot.PackagesSHA256 != "other" || p.runs["sub"] == nil ||
		p.runs["sub"].data.latest["A"] == nil {
		t.Errorf("results after a new snapshot: %v", err)
	}
	// Not the reference set of another perl version.
	writeFiles(t, subject, map[string]string{summaryFile: `{"perl":"5.42.0"}`})
	if err := exportReferenceSet(subject, results); err == nil {
		t.Errorf("the reference set of another perl version replaced the one of the runs there")
	}
}
