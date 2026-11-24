package main

// The cpanfile checker's script (site/assets/cpanfile.js), run in a
// JavaScript engine.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// checker loads cpanfile.js into a new JavaScript runtime.
func checker(t *testing.T) *goja.Runtime {
	t.Helper()
	src, err := siteFiles.ReadFile("site/assets/cpanfile.js")
	if err != nil {
		t.Fatal(err)
	}
	vm := goja.New()
	if _, err := vm.RunScript("cpanfile.js", string(src)); err != nil {
		t.Fatal(err)
	}
	return vm
}

// callJSON calls CPANfileCheck.<fn>(args...) and decodes its result, as
// JSON, into out.
func callJSON(t *testing.T, vm *goja.Runtime, out any, fn string, args ...any) {
	t.Helper()
	for i, a := range args {
		if err := vm.Set("arg"+string(rune('0'+i)), a); err != nil {
			t.Fatal(err)
		}
	}
	names := make([]string, len(args))
	for i := range args {
		names[i] = "arg" + string(rune('0'+i))
	}
	v, err := vm.RunString("JSON.stringify(CPANfileCheck." + fn + "(" + strings.Join(names, ", ") + "))")
	if err != nil {
		t.Fatalf("%s: %v", fn, err)
	}
	if err := json.Unmarshal([]byte(v.String()), out); err != nil {
		t.Fatalf("%s: %v: %s", fn, err, v.String())
	}
}

func TestCheckerVersionsAgreeWithGo(t *testing.T) {
	vm := checker(t)
	for _, tc := range []struct{ have, want string }{
		{"1.302219", "0.88"}, {"1.302219", "1.302220"}, {"1.3", "1.29"}, {"1.10", "1.9"},
		{"v6.0.2", "4.14"}, {"0.25", "v0.250.0"}, {"0.25", "v0.251.0"}, {"1.70", ">= 1.45, < 2.0"},
		{"2.01", ">= 1.45, < 2.0"}, {"1.50", "!= 1.50"}, {"1.23_01", "1.23"}, {"", ""}, {"", "0"},
		{"", "1.0"}, {"1.0", "abc"}, {"1.0-TRIAL", "0.5"}, {"1.002003", "== v1.2.3"}, {"1.2.3", "> 1.002002"},
		{"5.44.0", "5.010"}, {"5.44.0", "5.046"}, {"1.0", "<= 1.0"}, {"1.0", "< 1.0"}, {".5", "0.4"},
		{"1.", "1"}, {"v1.2", "v1.2.0"}, {" 2.0 ", " >=  1.5 "}, {"1.2.3", ">= 1.2.3, != 1.2.3"},
	} {
		var js struct{ Satisfied, OK bool }
		callJSON(t, vm, &js, "satisfies", tc.have, tc.want)
		satisfied, ok := satisfies(tc.have, tc.want)
		if js.Satisfied != satisfied || js.OK != ok {
			t.Errorf("satisfies(%q, %q): JavaScript %v, %v; Go %v, %v", tc.have, tc.want, js.Satisfied, js.OK, satisfied, ok)
		}
	}
}

func TestCheckerParsesCPANfiles(t *testing.T) {
	vm := checker(t)
	text := `# a comment
requires 'Mojolicious', '9.0';
requires "JSON::XS" => ">= 4.0, < 5";
requires Plack => 1.0047, dist => 'MIYAGAWA/Plack-1.0047.tar.gz';
requires q{Moo}, q(2.004),;
recommends 'Cpanel::JSON::XS';
test_requires 'Test::More', '0.98';

=pod

requires 'Inside::Pod';

=cut

on 'test' => sub {
    requires 'Test::Deep';
    suggests 'Test::Pod';
};
on develop => sub { requires 'Dist::Zilla' };
feature 'sqlite', 'SQLite support' => sub {
    requires 'DBD::SQLite', '1.70';
    on runtime => sub { requires 'DBI' };
};
requires "Interpolated::$name";
requires $module;
mirror 'http://example.com/';
requires 'Fine';
__END__
requires 'After::End';
`
	var got struct {
		Requirements []struct {
			Module, Version, Phase, Relationship, Feature string
			Line                                          int
		}
		Unparsed []struct {
			Line, End int
			Text      string
		}
	}
	callJSON(t, vm, &got, "parseCPANfile", text)
	var reqs []string
	for _, r := range got.Requirements {
		reqs = append(reqs, strings.Join([]string{r.Module, r.Version, r.Phase, r.Relationship, r.Feature}, " ")+" @"+string(rune('0'+r.Line/10))+string(rune('0'+r.Line%10)))
	}
	want := []string{
		"Mojolicious 9.0 runtime requires  @02",
		"JSON::XS >= 4.0, < 5 runtime requires  @03",
		"Plack 1.0047 runtime requires  @04",
		"Moo 2.004 runtime requires  @05",
		"Cpanel::JSON::XS 0 runtime recommends  @06",
		"Test::More 0.98 test requires  @07",
		"Test::Deep 0 test requires  @16",
		"Test::Pod 0 test suggests  @17",
		"Dist::Zilla 0 develop requires  @19",
		"DBD::SQLite 1.70 runtime requires sqlite @21",
		"DBI 0 runtime requires sqlite @22",
		"Fine 0 runtime requires  @27",
	}
	if !reflect.DeepEqual(reqs, want) {
		t.Errorf("requirements:\n%s\nwant:\n%s", strings.Join(reqs, "\n"), strings.Join(want, "\n"))
	}
	var unparsed []string
	for _, u := range got.Unparsed {
		unparsed = append(unparsed, u.Text)
	}
	wantUnparsed := []string{`requires "Interpolated::$name";`, `requires $module;`, `mirror 'http://example.com/';`}
	if !reflect.DeepEqual(unparsed, wantUnparsed) {
		t.Errorf("unparsed: %q, want %q", unparsed, wantUnparsed)
	}
	if len(got.Unparsed) == 3 && (got.Unparsed[0].Line != 24 || got.Unparsed[2].End != 26) {
		t.Errorf("unparsed lines: %+v", got.Unparsed)
	}
}

func TestCheckerReportsWhatItCannotRead(t *testing.T) {
	vm := checker(t)
	for text, wantLines := range map[string]string{
		// A statement that runs over its block, a missing semicolon, a
		// list operator, an unknown phase.
		"requires 'A'\nrequires 'B';\n":            "1-2",
		"requires $_ for qw(A B);\n":               "1",
		"on 'nonsense' => sub { requires 'X' };\n": "1",
		"requires 'A', '1', 'dangling';\n":         "1",
		"}\nrequires 'B';\n":                       "1",
	} {
		var got struct {
			Requirements []struct{ Module string }
			Unparsed     []struct{ Line, End int }
		}
		callJSON(t, vm, &got, "parseCPANfile", text)
		if len(got.Unparsed) == 0 {
			t.Errorf("%q: nothing reported", text)
			continue
		}
		u := got.Unparsed[0]
		lines := string(rune('0' + u.Line))
		if u.End != u.Line {
			lines += "-" + string(rune('0'+u.End))
		}
		if lines != wantLines {
			t.Errorf("%q: reported lines %s, want %s", text, lines, wantLines)
		}
	}
}

func TestCheckerParsesSnapshots(t *testing.T) {
	vm := checker(t)
	text := "# carton snapshot format: version 1.0\nDISTRIBUTIONS\n  Moo-2.005005\n    pathname: H/HA/HAARG/Moo-2.005005.tar.gz\n" +
		"    provides:\n      Moo 2.005005\n      Moo::_Utils undef\n    requirements:\n      Role::Tiny 2.002004\n"
	var got struct {
		Distributions []struct {
			Name, Pathname string
			Provides       [][2]string
		}
		Errors []struct{ Line int }
	}
	callJSON(t, vm, &got, "parseSnapshot", text)
	if len(got.Distributions) != 1 || got.Distributions[0].Name != "Moo-2.005005" || got.Distributions[0].Pathname != "H/HA/HAARG/Moo-2.005005.tar.gz" ||
		!reflect.DeepEqual(got.Distributions[0].Provides, [][2]string{{"Moo", "2.005005"}, {"Moo::_Utils", ""}}) || len(got.Errors) != 0 {
		t.Errorf("parseSnapshot: %+v", got)
	}
	var is bool
	callJSON(t, vm, &is, "isSnapshot", text)
	if !is {
		t.Errorf("a snapshot is not one")
	}
}

// checkerData is check.json of a small survey: App needs Lib, which needs
// Base; Broken failed; XS links libssl; Bundled ships with the interpreter
// in a version too old for one requirement.
func checkerData(t *testing.T) *checkData {
	t.Helper()
	latest := []*result{
		{Dist: "App", Outcome: outcomePass}, {Dist: "Lib", Outcome: outcomePass}, {Dist: "Base", Outcome: outcomePass},
		{Dist: "Broken", Outcome: outcomeFail}, {Dist: "XS", Outcome: outcomePass}, {Dist: "NeedsBroken", Outcome: outcomePass},
	}
	run := &siteRun{
		Name: "sub", data: newRunData("sub", latest, nil),
		static: map[string]*staticRecord{
			"App":         {Dist: "App", State: staticStatic, Closure: []string{"App", "Base", "Lib"}},
			"Lib":         {Dist: "Lib", State: staticStatic},
			"XS":          {Dist: "XS", State: staticStatic, Reason: reasonBundlesSystemLibraries, ExternalLibs: []externalLib{{Name: "ssl", Dist: "XS", StaticArchive: true}}},
			"NeedsBroken": {Dist: "NeedsBroken", State: staticBlocked, BlockedBy: []string{"Broken"}},
		},
		provided: map[string]string{"Bundled": "1.0", "strict": ""},
	}
	s := &siteData{
		Snapshot: publishedSnapshot{Perl: "5.44.0"}, Subject: run,
		packages: []packageDist{
			{Dist: "App", Release: "App-1.0", Modules: [][2]string{{"App", "1.0"}}, Needs: []string{"Lib"}},
			{Dist: "Lib", Release: "Lib-2.0", Modules: [][2]string{{"Lib", "2.0"}, {"Lib::Util", ""}}, Needs: []string{"Base"}},
			{Dist: "Base", Release: "Base-0.5", Modules: [][2]string{{"Base", "0.5"}}},
			{Dist: "Broken", Release: "Broken-1", Modules: [][2]string{{"Broken", "1"}}},
			{Dist: "XS", Release: "XS-3", Modules: [][2]string{{"XS", "3"}}},
			{Dist: "NeedsBroken", Release: "NeedsBroken-1", Modules: [][2]string{{"NeedsBroken", "1"}}, Needs: []string{"Broken"}},
			{Dist: "Bundled", Release: "Bundled-2.0", Modules: [][2]string{{"Bundled", "2.0"}}},
			{Dist: "Unsurveyed", Release: "Unsurveyed-1", Modules: [][2]string{{"Unsurveyed", "1"}}},
		},
		pages: map[string]string{"App": "App", "Lib": "Lib"},
	}
	c, err := checkDataOf(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type checkRow struct {
	Kind, Name, Range string
	Verdict           struct {
		Dist *struct {
			Name, Release, Outcome, Page string
		}
		Supported, Static string
		Notes             []string
		Libraries         []string
	}
}

type checkResult struct {
	Rows, Requirements []checkRow
	Others             []struct{ Module, Relationship, Phase string }
	Unparsed           []struct{ Line int }
	Supported, Static  string
	Libraries          []string
}

func runCheck(t *testing.T, cpanfile, snapshot string) checkResult {
	t.Helper()
	vm := checker(t)
	raw, err := json.Marshal(checkerData(t))
	if err != nil {
		t.Fatal(err)
	}
	data, err := vm.RunString("(" + string(raw) + ")")
	if err != nil {
		t.Fatal(err)
	}
	var out checkResult
	callJSON(t, vm, &out, "check", data, cpanfile, snapshot)
	return out
}

func TestCheckerJudgesRequirements(t *testing.T) {
	verdicts := func(out checkResult) map[string]string {
		m := map[string]string{}
		for _, r := range out.Rows {
			m[r.Name] = r.Verdict.Supported + "/" + r.Verdict.Static
		}
		return m
	}
	out := runCheck(t, "requires 'App', '1.0';\nrequires 'XS';\nrequires 'perl', '5.010';\nrequires 'Bundled', '0.9';\n"+
		"recommends 'Broken';\non develop => sub { requires 'Broken' };\n", "")
	if want := map[string]string{"App": "yes/yes", "XS": "yes/yes", "perl": "yes/yes", "Bundled": "yes/yes"}; !reflect.DeepEqual(verdicts(out), want) {
		t.Errorf("verdicts %v, want %v", verdicts(out), want)
	}
	if out.Supported != "yes" || out.Static != "yes" || !reflect.DeepEqual(out.Libraries, []string{"ssl"}) || len(out.Others) != 2 {
		t.Errorf("overall %s/%s, libraries %v, others %v", out.Supported, out.Static, out.Libraries, out.Others)
	}

	out = runCheck(t, "requires 'Lib', '>= 3';\nrequires 'Unsurveyed';\nrequires 'Nowhere';\nrequires 'Bundled', '2.0';\n", "")
	if want := map[string]string{"Lib": "unknown/unknown", "Unsurveyed": "unknown/unknown", "Nowhere": "unknown/unknown", "Bundled": "unknown/unknown"}; !reflect.DeepEqual(verdicts(out), want) {
		t.Errorf("verdicts %v, want %v", verdicts(out), want)
	}
	if out.Supported != "unknown" || out.Static != "unknown" {
		t.Errorf("overall %s/%s, want unknown", out.Supported, out.Static)
	}

	// Lib passed, and its static build was measured; Broken did not pass,
	// and NeedsBroken, which passed, needs it.
	out = runCheck(t, "requires 'Lib::Util';\nrequires 'Broken';\nrequires 'NeedsBroken';\n", "")
	if want := map[string]string{"Lib::Util": "yes/yes", "Broken": "no/unknown", "NeedsBroken": "no/no"}; !reflect.DeepEqual(verdicts(out), want) {
		t.Errorf("verdicts %v, want %v", verdicts(out), want)
	}
	if out.Supported != "no" || out.Static != "no" {
		t.Errorf("overall %s/%s, want no", out.Supported, out.Static)
	}
	for _, r := range out.Rows {
		if r.Name == "Lib::Util" && (r.Verdict.Dist == nil || r.Verdict.Dist.Page != "Lib" || r.Verdict.Dist.Release != "Lib-2.0") {
			t.Errorf("Lib::Util's distribution: %+v", r.Verdict.Dist)
		}
	}
}

func TestCheckerJudgesSnapshots(t *testing.T) {
	snapshot := "# carton snapshot format: version 1.0\nDISTRIBUTIONS\n" +
		"  App-1.0\n    pathname: A/AU/AUTHOR/App-1.0.tar.gz\n    provides:\n      App 1.0\n    requirements:\n      Lib 0\n" +
		"  Lib-1.9\n    pathname: A/AU/AUTHOR/Lib-1.9.tar.gz\n    provides:\n      Lib 1.9\n    requirements:\n"
	out := runCheck(t, "requires 'App';\nrequires 'Other';\n", snapshot)
	got := map[string]string{}
	for _, r := range out.Rows {
		got[r.Kind+" "+r.Name] = r.Verdict.Supported
	}
	// The snapshot pins another release of Lib than the survey measured;
	// Other is not in it.
	want := map[string]string{"release App-1.0": "yes", "release Lib-1.9": "unknown", "requirement Other": "unknown"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows %v, want %v", got, want)
	}
	if out.Supported != "unknown" || len(out.Requirements) != 2 {
		t.Errorf("overall %s, requirements %+v", out.Supported, out.Requirements)
	}
}

func TestCheckerUnreadLinesMakeVerdictsUnknown(t *testing.T) {
	// A v-string version is read; a list operator is not, and what it may
	// require keeps the verdict from yes.
	out := runCheck(t, "requires 'App', v1.0.0;\nrequires $_ for qw(XS Broken);\n", "")
	if len(out.Rows) != 1 || out.Rows[0].Range != "v1.0.0" || out.Rows[0].Verdict.Supported != "yes" || len(out.Unparsed) != 1 {
		t.Fatalf("rows %+v, unparsed %+v", out.Rows, out.Unparsed)
	}
	if out.Supported != "unknown" || out.Static != "unknown" {
		t.Errorf("overall %s/%s, want unknown", out.Supported, out.Static)
	}
	if out = runCheck(t, "requires 'App' # a comment\n  ;\nrequires q #x#;\n", ""); len(out.Unparsed) != 1 {
		t.Errorf("q followed by a comment was read: %+v", out)
	}
}
