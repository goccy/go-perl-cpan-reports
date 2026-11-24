package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPageNames(t *testing.T) {
	pages := pageNames(map[string]bool{"Moose": true, "Text-Tabs+Wrap": true, "Alien-Xmake": true, "Alien-xmake": true, "LMDB_File": true})
	if pages["Moose"] != "Moose" || pages["LMDB_File"] != "LMDB_File" {
		t.Errorf("plain names changed: %v", pages)
	}
	seen := map[string]string{}
	for name, p := range pages {
		if !plainDistName.MatchString(strings.ReplaceAll(p, "~", "-")) {
			t.Errorf("page of %q is %q", name, p)
		}
		lower := strings.ToLower(p)
		if other, dup := seen[lower]; dup {
			t.Errorf("%q and %q have pages that differ only in case: %q", name, other, p)
		}
		seen[lower] = name
	}
	if !strings.HasPrefix(pages["Text-Tabs+Wrap"], "Text-Tabs_Wrap~") {
		t.Errorf("page of Text-Tabs+Wrap is %q", pages["Text-Tabs+Wrap"])
	}
}

func TestFormatting(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 14318: "14,318", 1234567: "1,234,567", -1500: "-1,500"} {
		if got := formatCount(n); got != want {
			t.Errorf("formatCount(%d) = %q, want %q", n, got, want)
		}
	}
	for n, want := range map[int]string{0: "0 s", 59: "59 s", 300: "5 min", 139: "2 min 19 s", 7200: "2 h", 7290: "2 h 1 min"} {
		if got := formatSeconds(n); got != want {
			t.Errorf("formatSeconds(%d) = %q, want %q", n, got, want)
		}
	}
	if got := formatDateRange("2026-11-14T18:31:53Z", "2026-11-14T22:27:05Z"); got != "2026-11-14" {
		t.Errorf("formatDateRange of one day = %q", got)
	}
}

// writeResults makes a results directory of a subject run whose logs and
// test file names hold markup, and, when withReference is set, of a
// reference run it is compared with.
func writeResults(t *testing.T, withReference bool) string {
	t.Helper()
	dists := []dist{
		{Dist: "Evil", Release: "Evil-1", DependentsTotal: 1, OnPerl: &perlReports{LinuxPass: 3}},
		{Dist: "Good", Release: "Good-1", OnPerl: &perlReports{LinuxPass: 1}},
		{Dist: "Text-Tabs+Wrap", Release: "Text-Tabs+Wrap-1", OnPerl: &perlReports{LinuxPass: 1}},
		{Dist: "Dep", Release: "Dep-1", DependentsTotal: 1},
	}
	snapshot := `{"fetched_at":"2026-01-01T00:00:00Z","packages_url":"u","packages_sha256":"abc","releases":4}`
	inv := `{"image":"i","harness":{"module":"m"},"jobs":1,"cpus":"1","memory":"1g","wall_seconds":60,"test_seconds":30}` + "\n"
	subject := writeDataDir(t, dists, "sub", []result{
		{Dist: "Evil", Outcome: outcomeFail, Phases: []phaseRecord{{Phase: "build"}, {Phase: "test", Status: 1}},
			Tests: []testFile{{File: "t/<b>x</b>.t", Failed: 1, Exit: 1}}, Finished: "2026-01-02T00:00:00Z"},
		{Dist: "Good", Outcome: outcomePass, Finished: "2026-01-02T00:00:00Z"},
		{Dist: "Text-Tabs+Wrap", Outcome: outcomeBlocked, BlockedBy: []string{"Dep"}, Finished: "2026-01-02T00:00:00Z"},
		{Dist: "Dep", Outcome: outcomeBuildFailed, Phases: []phaseRecord{{Phase: "build", Status: 2}}, Finished: "2026-01-02T00:00:00Z"},
	}, map[string]string{
		"Evil/test.log": "t/<b>x</b>.t .. \n<script>alert(1)</script> at t/x.t line 3.\n</pre><img src=x onerror=alert(2)>\n",
	})
	reference := writeDataDir(t, dists, "ref", []result{
		{Dist: "Evil", Outcome: outcomePass}, {Dist: "Good", Outcome: outcomePass},
		{Dist: "Text-Tabs+Wrap", Outcome: outcomePass}, {Dist: "Dep", Outcome: outcomePass},
	}, nil)
	for _, dir := range []string{subject, reference} {
		writeFiles(t, dir, map[string]string{snapshotFile: snapshot})
	}
	writeFiles(t, subject, map[string]string{"runs/sub/invocations.jsonl": inv})
	writeFiles(t, reference, map[string]string{"runs/ref/invocations.jsonl": inv})
	results := filepath.Join(t.TempDir(), "results")
	if err := exportReferenceSet(reference, results); err != nil {
		t.Fatal(err)
	}
	runs := []exportOptions{{dataDir: subject, run: "sub", results: results,
		interpreter: interpreterMeta{Name: "go-perl", Commit: "abcdef0123"}, provisional: true, note: "a <trial> run"}}
	if withReference {
		runs[0].comparedWith = "ref"
		runs = append(runs, exportOptions{dataDir: reference, run: "ref", results: results, interpreter: interpreterMeta{Name: "perl", Version: "5.44.0"}})
	}
	for _, opts := range runs {
		if err := exportRun(opts); err != nil {
			t.Fatal(err)
		}
	}
	return results
}

func TestGenerateSiteWithoutReferenceRun(t *testing.T) {
	results := writeResults(t, false)
	out := filepath.Join(t.TempDir(), "site")
	if err := generateSite(results, out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "<div class=\"value\">33.3%</div>") || strings.Contains(string(raw), "against") {
		t.Errorf("index.html without a reference run:\n%s", raw)
	}
}

func TestGenerateSite(t *testing.T) {
	results := writeResults(t, true)
	out := filepath.Join(t.TempDir(), "site")
	if err := generateSite(results, out); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	index := read("index.html")
	for _, want := range []string{
		"Provisional results.", "a &lt;trial&gt; run",
		"<div class=\"value\">33.3%</div>", // 1 of 3 passes on go-perl
		"<div class=\"value\">100.0%</div>",
		`href="d/Dep.html"`, // what blocks the most
		`<script src="assets/theme.js" defer></script>`, `<button type="button" id="theme" class="theme" hidden></button>`,
	} {
		if !strings.Contains(index, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	evil := read("d/Evil.html")
	for _, bad := range []string{"<script>alert", "<img src=x", "<b>x</b>"} {
		if strings.Contains(evil, bad) {
			t.Errorf("d/Evil.html has %q unescaped", bad)
		}
	}
	for _, want := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "&lt;/pre&gt;&lt;img src=x onerror=alert(2)&gt;", "t/&lt;b&gt;x&lt;/b&gt;.t"} {
		if !strings.Contains(evil, want) {
			t.Errorf("d/Evil.html lacks %q", want)
		}
	}
	for _, rel := range []string{`href="../assets/style.css"`, `src="../assets/theme.js"`, `href="../index.html"`} {
		if !strings.Contains(evil, rel) {
			t.Errorf("d/Evil.html lacks the relative link %s", rel)
		}
	}
	if strings.Contains(index+evil, `href="/`) || strings.Contains(index+evil, `src="/`) {
		t.Errorf("a page links by an absolute path")
	}

	var table distTable
	if err := json.Unmarshal([]byte(read("dists.json")), &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Rows) != 3 || table.Subject != "sub" || table.Reference != "ref" {
		t.Fatalf("dists.json: %+v", table)
	}
	pages := map[string]any{}
	for _, row := range table.Rows {
		pages[row[0].(string)] = row[7]
	}
	if pages["Evil"] != nil || !strings.HasPrefix(pages["Text-Tabs+Wrap"].(string), "Text-Tabs_Wrap~") {
		t.Errorf("pages in dists.json: %v", pages)
	}
	if _, err := os.Stat(filepath.Join(out, "d", pages["Text-Tabs+Wrap"].(string)+".html")); err != nil {
		t.Errorf("the page of Text-Tabs+Wrap: %v", err)
	}
	for _, name := range []string{"dists.html", "methodology.html", "assets/style.css", "assets/dists.js", "assets/theme.js", ".nojekyll", "d/Dep.html"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
