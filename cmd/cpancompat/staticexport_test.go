package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestStaticThroughExportAndSite exports a run with a static build phase
// and generates its site.
func TestStaticThroughExportAndSite(t *testing.T) {
	dists := []dist{
		{Dist: "Good", Release: "Good-1", Modules: []string{"Good"}, OnPerl: &perlReports{LinuxPass: 1}},
		{Dist: "Net-SSLeay", Release: "Net-SSLeay-1.94", Modules: []string{"Net::SSLeay"}, OnPerl: &perlReports{LinuxPass: 1},
			Requires: []requirement{{Module: "Good", Phase: "runtime", Relationship: "requires", Dist: "Good"},
				{Module: "Test::More", Phase: "test", Relationship: "requires", Dist: "Test-Simple"},
				{Module: "strict", Phase: "runtime", Relationship: "requires", Dist: "perl", Provided: true}}},
		{Dist: "Needy", Release: "Needy-1", Modules: []string{"Needy"}, OnPerl: &perlReports{LinuxPass: 1}},
	}
	data := writeDataDir(t, dists, "sub", []result{
		{Dist: "Good", Outcome: outcomePass, Finished: "2026-01-02T00:00:00Z"},
		{Dist: "Net-SSLeay", Outcome: outcomePass, Finished: "2026-01-02T00:00:00Z"},
		{Dist: "Needy", Outcome: outcomePass, Finished: "2026-01-02T00:00:00Z"},
	}, nil)
	statics := []staticRecord{
		{Dist: "Good", Release: "Good-1", State: staticStatic, Loaded: []string{"Good"}, Closure: []string{"Good"},
			Binary: &binaryInfo{Bytes: 41000000, Needed: []string{}}, Finished: "2026-01-03T00:00:00Z"},
		{Dist: "Net-SSLeay", Release: "Net-SSLeay-1.94", State: staticStatic, Reason: reasonBundlesSystemLibraries,
			Closure: []string{"Good", "Net-SSLeay"}, NativeModules: []staticNative{{"Net::SSLeay", "Net-SSLeay"}},
			ExternalLibs: []externalLib{{Name: "ssl", Dist: "Net-SSLeay", From: "system", StaticArchive: true}, {Name: "crypto", Dist: "Net-SSLeay", From: "system", StaticArchive: true}},
			Finished:     "2026-01-03T00:00:01Z"},
		{Dist: "Needy", Release: "Needy-1", State: staticNotStatic, Reason: reasonNeedsEnvironment, Closure: []string{"Needy"},
			Phases:   []phaseRecord{{Phase: "modules"}, {Phase: "link"}, {Phase: "run_empty", Status: 1}, {Phase: "run_control"}},
			Finished: "2026-01-03T00:00:02Z"},
	}
	if err := writeJSONLines(filepath.Join(data, "runs", "sub", "static.jsonl"), statics); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, data, map[string]string{
		snapshotFile:                                `{"fetched_at":"2026-01-01T00:00:00Z","packages_url":"u","packages_sha256":"abc","releases":3}`,
		"runs/sub/invocations.jsonl":                `{"image":"i","harness":{"module":"m"},"jobs":1,"cpus":"1","memory":"1g","wall_seconds":60,"test_seconds":30}` + "\n",
		"runs/sub/static-invocations.jsonl":         `{"started":"2026-01-03T00:00:00Z","image":"s","platform":"linux/arm64","harness":{"module":"m"},"jobs":4,"cpus":"2","memory":"4g","wall_seconds":3600}` + "\n",
		"runs/sub/static/probe/gperl.json":          `{"module":"github.com/goccy/go-perl","version":"v0.3.1"}`,
		"runs/sub/static/probe/versions.json":       `{"cc":"gcc 12.2.0","libc":"glibc 2.36","go":"go1.26.4"}`,
		"runs/sub/static/dists/Needy/run_empty.log": "load.pl: Needy: cannot open /etc/needy.conf at " + data + "/x line 1.\n",
	})
	results := filepath.Join(t.TempDir(), "results")
	if err := exportReferenceSet(data, results); err != nil {
		t.Fatal(err)
	}
	if err := exportRun(exportOptions{dataDir: data, run: "sub", results: results, interpreter: interpreterMeta{Name: "go-perl", Commit: "c0ffee"}}); err != nil {
		t.Fatal(err)
	}
	packages, err := readGzipJSONLines[packageDist](filepath.Join(results, packagesGzFile))
	if err != nil {
		t.Fatal(err)
	}
	// Test requirements and what the interpreter ships are not needs.
	if want := (packageDist{Dist: "Net-SSLeay", Release: "Net-SSLeay-1.94", Modules: [][2]string{{"Net::SSLeay", "1.0"}}, Needs: []string{"Good"}}); !reflect.DeepEqual(packages[1], want) {
		t.Errorf("packages: %+v", packages)
	}
	p, err := readPublished(results)
	if err != nil {
		t.Fatal(err)
	}
	pr := p.runs["sub"]
	if len(pr.static) != 3 || pr.meta.Static == nil || pr.meta.Static.Gperl.Version != "v0.3.1" || pr.meta.Static.Recorded != 3 ||
		len(pr.meta.Static.Invocations) != 1 || pr.provided["strict"] != "1.13" {
		t.Fatalf("published run: static %d, meta %+v, provided %v", len(pr.static), pr.meta.Static, pr.provided)
	}
	ex := pr.staticLogs["Needy"]
	if ex == nil || ex.Log != "run_empty.log" || strings.Contains(ex.Text, data) {
		t.Errorf("Needy's log: %+v", ex)
	}

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
	ssl := read("d/Net-SSLeay.html")
	for _, want := range []string{"Static build", "Static (links system libraries statically)", "<code>libssl</code>", "<code>libcrypto</code>"} {
		if !strings.Contains(ssl, want) {
			t.Errorf("d/Net-SSLeay.html lacks %q", want)
		}
	}
	if needy := read("d/Needy.html"); !strings.Contains(needy, "Not static: "+staticReasonTitles[reasonNeedsEnvironment]) || !strings.Contains(needy, "/etc/needy.conf") {
		t.Errorf("d/Needy.html:\n%s", needy)
	}
	if index := read("index.html"); !strings.Contains(index, "Static builds") || !strings.Contains(index, `href="dists.html#static=static%3abundles_system_libraries"`) {
		t.Errorf("index.html has no static build counts")
	}
	if m := read("methodology.html"); !strings.Contains(m, `id="static"`) || !strings.Contains(m, `id="checker"`) {
		t.Errorf("methodology.html lacks the static build or the checker")
	}
	if !strings.Contains(read("check.html"), `src="assets/cpanfile.js"`) || !strings.Contains(read("index.html"), `href="check.html"`) {
		t.Errorf("no checker page or link")
	}
	var table distTable
	if err := json.Unmarshal([]byte(read("dists.json")), &table); err != nil {
		t.Fatal(err)
	}
	static := map[string]string{}
	for _, row := range table.Rows {
		if i := int(row[8].(float64)); i >= 0 {
			static[row[0].(string)] = table.Static[i].ID
		}
	}
	if want := map[string]string{"Good": "static", "Net-SSLeay": "static:" + reasonBundlesSystemLibraries, "Needy": "not_static:" + reasonNeedsEnvironment}; !reflect.DeepEqual(static, want) {
		t.Errorf("static in dists.json: %v", static)
	}
	var check checkData
	if err := json.Unmarshal([]byte(read("check.json")), &check); err != nil {
		t.Fatal(err)
	}
	if len(check.Dists) != 3 || len(check.Modules) != 3 || check.Perl != "5.44.0" || check.Stdlib["strict"] != "1.13" {
		t.Errorf("check.json: %+v", check)
	}
}
