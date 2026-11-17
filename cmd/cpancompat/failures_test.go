package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCauseOf(t *testing.T) {
	for _, tc := range []struct {
		evidence []string
		outcome  string
		want     cause
	}{
		{[]string{"Foo.xs:3:10: fatal error: gtk/gtk.h: No such file or directory"}, outcomeBuildFailed, causeExternalLibrary},
		// A header of perl itself is not an external library.
		{[]string{"Foo.xs:1:10: fatal error: perliol.h: No such file or directory"}, outcomeBuildFailed, causePerlHeader},
		{[]string{"Foo.xs:1:10: fatal error: EXTERN.h: No such file or directory"}, outcomeBuildFailed, causePerlHeader},
		{[]string{"/usr/bin/ld: cannot find -lgdbm"}, outcomeBuildFailed, causeExternalLibrary},
		{[]string{"OS unsupported"}, outcomeConfigureFailed, causeOtherOS},
		{[]string{"LMDB.xs:138:13: error: implicit declaration of function ‘Perl_sv_utf8_downgrade_flags’"}, outcomeBuildFailed, causeCAPI},
		{[]string{"This Perl not built to support threads"}, outcomeFail, causeThreads},
		{[]string{"Can't locate Foo/Bar.pm in @INC (you may need to install the Foo::Bar module)"}, outcomeFail, causeModuleNotFound},
		{[]string{"Can't locate strict.pm:   /home/cpan/.cache/x/strict.pm: Permission denied at t/a.t line 3."}, outcomeFail, causeINCUnreadable},
		{[]string{"panic: something at t/a.t line 4."}, outcomeFail, causeCrash},
		{[]string{"Temporary failure in name resolution"}, outcomeFail, causeNetwork},
		// go-perl's message when a forked child was to run native code, for
		// an XSUB, a library to load and a hook.
		{[]string{"Foo::bar: native modules cannot be used in a forked child (/opt/lib/auto/Foo/Foo.so) at t/fork.t line 9."},
			outcomeFail, causeForkChildNativeCode},
		{[]string{"Can't load '/opt/lib/auto/IO/Tty/Tty.so' for module IO::Tty: /opt/lib/auto/IO/Tty/Tty.so: native modules cannot be used in a forked child at /usr/lib/DynaLoader.pm line 206."},
			outcomeFail, causeForkChildNativeCode},
		{[]string{"the op checker of a native module: native modules cannot be used in a forked child at /opt/lib/Hash/Util.pm line 13."},
			outcomeFail, causeForkChildNativeCode},
		// The module it names does not make it TLS.
		{[]string{"Net::SSLeay::CTX_new: native modules cannot be used in a forked child (/opt/lib/auto/Net/SSLeay/SSLeay.so) at t/a.t line 3."},
			outcomeFail, causeForkChildNativeCode},
		// Where the evidence also shows a crash, it does not apply, wherever
		// the crash is, and a cause after it in the order does.
		{[]string{"SIGSEGV: segmentation violation", "Foo::bar: native modules cannot be used in a forked child at t/fork.t line 9."},
			outcomeFail, causeCrash},
		{[]string{"Net::SSLeay::CTX_new: native modules cannot be used in a forked child at t/a.t line 3.", "panic: x"},
			outcomeFail, causeTLSOrPTY},
		// The first cause that matches wins.
		{[]string{"panic: x", "Can't locate Foo.pm in @INC"}, outcomeFail, causeModuleNotFound},
		{nil, outcomeOutputLimit, causeOutputLimit},
		{[]string{"t/a.t .. ok"}, outcomeTimeout, causeTimeLimit},
		{[]string{"#   Failed test 'x'"}, outcomeFail, causeNotAnalysed},
		{nil, outcomeDriverFailed, causeNotAnalysed},
	} {
		if got := causeOf(tc.evidence, tc.outcome); got != tc.want {
			t.Errorf("causeOf(%q, %s) = %s, want %s", tc.evidence, tc.outcome, got, tc.want)
		}
	}
}

func TestEveryCauseHasATitle(t *testing.T) {
	seen := map[cause]bool{}
	for _, c := range causes {
		if causeTitles[c] == "" {
			t.Errorf("cause %s has no title", c)
		}
		seen[c] = true
	}
	for _, c := range evidenceCauses {
		if !seen[c.cause] {
			t.Errorf("cause %s is not listed in causes", c.cause)
		}
	}
	for _, c := range stuckCauses {
		if !seen[c] {
			t.Errorf("cause %s is not listed in causes", c)
		}
	}
}

func TestSplitLines(t *testing.T) {
	got := splitLines("a\nb\r\nc\rd\x0ce f")
	want := []string{"a", "b", "c", "d", "e", "f"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitLines = %q, want %q", got, want)
	}
	if got := splitLines("a\n"); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("splitLines(a\\n) = %q", got)
	}
}

func TestNormalLine(t *testing.T) {
	got := normalLine("at /work/Foo-1.0-AbC123/Foo-1.0/blib/lib/Foo.pm line 12, 0xdeadBEEF /home/cpan/.cache/go-perl/stdlib-0a1b/strict.pm")
	want := "at blib/lib/Foo.pm line N, 0xN stdlib/strict.pm"
	if got != want {
		t.Errorf("normalLine = %q, want %q", got, want)
	}
}

// writeFiles makes the named files below dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFailureRecordOfFailingTests(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"test.log": strings.Join([]string{
		"PERL_DL_NONLAZY=1 perl \"-MExtUtils::Command::MM\" ...",
		"t/00-load.t .. ok",
		"#   Failed test 'adds'",
		"#   at t/01-add.t line 12.",
		"#          got: '3'",
		"#     expected: '4'",
		"# Looks like you failed 1 test of 2.",
		"t/01-add.t ... ",
		"Dubious, test returned 1 (wstat 256, 0x100)",
		"Failed 1/2 subtests ",
		"Test Summary Report",
	}, "\n")})
	res := &result{
		Dist: "Foo", Release: "Foo-1.0", Outcome: outcomeFail, Interpreter: "go-perl@abc",
		Phases: []phaseRecord{{Phase: "unpack"}, {Phase: "configure"}, {Phase: "build"}, {Phase: "test", Status: 2}},
		Tests: []testFile{
			{File: "t/00-load.t", Planned: 1, Run: 1, Passed: 1},
			{File: "t/01-add.t", Planned: 2, Run: 2, Passed: 1, Failed: 1, Exit: 1, Wait: 256},
		},
	}
	d := &dist{Dist: "Foo", DependentsTotal: 7, OnPerl: &perlReports{LinuxPass: 3}}
	rec := failureRecord(dir, res, d)
	want := failure{
		Dist: "Foo", Release: "Foo-1.0", Outcome: outcomeFail, Interpreter: "go-perl@abc",
		DependentsTotal: 7, Reference: true, Phase: "test", PhaseStatus: 2,
		TestFiles: 2, FailingTests: res.Tests[1:],
		Evidence: []string{
			"#   Failed test 'adds'",
			"#   at t/01-add.t line 12.",
			"#          got: '3'",
			"#     expected: '4'",
			"Dubious, test returned 1 (wstat 256, 0x100)",
		},
		Cause:     causeNotAnalysed,
		Signature: "#   Failed test 'adds'",
	}
	if !reflect.DeepEqual(rec, want) {
		t.Errorf("record:\n got %+v\nwant %+v", rec, want)
	}
}

// TestFailureRecordOfForkChildNativeCode: go-perl's message from a forked
// child is evidence even where it does not end like perl's die messages
// (here a test that printed the error it caught), and it gives the cause.
func TestFailureRecordOfForkChildNativeCode(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"test.log": strings.Join([]string{
		"t/00-load.t .. ok",
		"child failed: Foo::bar: native modules cannot be used in a forked child (/opt/lib/auto/Foo/Foo.so)",
		"# Looks like your test exited with 255 just after 1.",
		"t/fork.t ..... ",
		"Dubious, test returned 255 (wstat 65280, 0xff00)",
	}, "\n")})
	res := &result{
		Dist: "Foo", Release: "Foo-1.0", Outcome: outcomeFail,
		Phases: []phaseRecord{{Phase: "configure"}, {Phase: "build"}, {Phase: "test", Status: 2}},
		Tests: []testFile{
			{File: "t/00-load.t", Planned: 1, Run: 1, Passed: 1},
			{File: "t/fork.t", Planned: 2, Run: 1, Passed: 1, Exit: 255, Wait: 65280},
		},
	}
	rec := failureRecord(dir, res, nil)
	if rec.Cause != causeForkChildNativeCode || len(rec.Evidence) == 0 ||
		!strings.Contains(rec.Evidence[0], forkChildNativeCodeMessage) {
		t.Errorf("cause %s, evidence %q", rec.Cause, rec.Evidence)
	}
}

// TestFailureRecordReadsEveryLineOfFailingFiles: the cause is read from every
// evidence line the harness printed for the failing files, not only from the
// 8 the record shows, and the line that tells it is shown too.
func TestFailureRecordReadsEveryLineOfFailingFiles(t *testing.T) {
	lines := []string{"t/00-load.t .. ok"}
	// Lines that differ in more than numbers, which like failures share.
	for i := range 10 {
		lines = append(lines, fmt.Sprintf("#   Failed test 'request %c'", 'a'+i))
	}
	refusal := "Guard::guard: native modules cannot be used in a forked child (/opt/lib/auto/Guard/Guard.so) at /opt/lib/AnyEvent/Socket.pm line 9."
	lines = append(lines, refusal, "t/server.t .... ", "Dubious, test returned 10 (wstat 2560, 0xa00)")
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"test.log": strings.Join(lines, "\n")})
	res := &result{
		Dist: "Foo", Release: "Foo-1.0", Outcome: outcomeFail,
		Phases: []phaseRecord{{Phase: "test", Status: 2}},
		Tests: []testFile{
			{File: "t/00-load.t", Planned: 1, Run: 1, Passed: 1},
			{File: "t/server.t", Planned: 10, Run: 10, Failed: 10, Exit: 10, Wait: 2560},
		},
	}
	rec := failureRecord(dir, res, nil)
	if rec.Cause != causeForkChildNativeCode {
		t.Errorf("cause %s, want %s", rec.Cause, causeForkChildNativeCode)
	}
	if len(rec.Evidence) != 9 || rec.Evidence[8] != refusal {
		t.Errorf("evidence %q: want the first 8 lines and the refusal", rec.Evidence)
	}
}

// TestFailureRecordOfForkChildTimeOut: a phase that ran out of time where the
// test file running at the limit printed go-perl's message takes the cause
// of the limitation (its child died and it waited), keeping what the
// processes did; a message from a file that had ended does not.
func TestFailureRecordOfForkChildTimeOut(t *testing.T) {
	stuckLines := strings.Join([]string{
		`{"pid":10,"ppid":1,"state":"S","elapsed_seconds":300,"cpu_percent":0,"wchan":"do_wait","args":["/usr/bin/timeout","--kill-after=10","300","make","test"],"fds":{},"thread_wchans":{}}`,
		`{"pid":11,"ppid":10,"state":"S","elapsed_seconds":299,"cpu_percent":0,"wchan":"do_wait","args":["make","test"],"fds":{},"thread_wchans":{}}`,
		`{"pid":13,"ppid":11,"state":"S","elapsed_seconds":200,"cpu_percent":0,"wchan":"do_wait","args":["/opt/gperl/bin/gperl","run","t/server.t"],"fds":{},"thread_wchans":{}}`,
	}, "\n")
	refusal := "HTTP::Parser::XS::parse_http_request: native modules cannot be used in a forked child (/opt/lib/auto/HTTP/Parser/XS/XS.so) at /opt/lib/HTTP/Server/PSGI.pm line 4."
	for _, tc := range []struct {
		name  string
		log   string
		cause cause
	}{
		{"running file", "t/00-load.t .. ok\nt/server.t .. \n" + refusal + "\n", causeForkChildNativeCode},
		{"ended file", "t/00-load.t .. \n" + refusal + "\nt/00-load.t .. ok\nt/server.t .. \n", causeTimeLimitBlocked},
		{"with a crash", "t/server.t .. \n" + refusal + "\nSIGSEGV: segmentation violation\n", causeTimeLimitBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"test.log": tc.log, "test.stuck.jsonl": stuckLines})
			res := &result{Dist: "Foo", Outcome: outcomeTimeout, Phases: []phaseRecord{{Phase: "build"}, {Phase: "test", Status: 124, TimedOut: true}}}
			rec := failureRecord(dir, res, nil)
			if rec.Cause != tc.cause || rec.Stuck == nil || rec.Stuck.Kind != "blocked" {
				t.Errorf("cause %s, stuck %+v; want %s and the processes", rec.Cause, rec.Stuck, tc.cause)
			}
			if tc.cause == causeForkChildNativeCode && !slices.Contains(rec.Evidence, refusal) {
				t.Errorf("evidence %q lacks the message", rec.Evidence)
			}
		})
	}
}

func TestFailureRecordOfBuildFailure(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"build.log": strings.Join([]string{
		"cc -c Foo.c",
		"Foo.xs:10:5: error: implicit declaration of function ‘Perl_foo’ [-Werror=implicit-function-declaration]",
		"Foo.xs:12:5: error: ‘PL_bar’ undeclared (first use in this function)",
		"make: *** [Makefile:340: Foo.o] Error 1",
	}, "\n")})
	res := &result{
		Dist: "Foo", Outcome: outcomeBuildFailed,
		// A configure script that exited non-zero after writing its build
		// script is not where the distribution failed.
		Phases: []phaseRecord{{Phase: "unpack"}, {Phase: "configure", Status: 1}, {Phase: "prerequisites"}, {Phase: "build", Status: 2}},
	}
	rec := failureRecord(dir, res, nil)
	if rec.Phase != "build" || rec.PhaseStatus != 2 || rec.Cause != causeCAPI {
		t.Errorf("phase %s status %d cause %s; want build, 2, %s", rec.Phase, rec.PhaseStatus, rec.Cause, causeCAPI)
	}
	if want := []string{"Perl_foo", "PL_bar"}; !reflect.DeepEqual(rec.Symbols, want) {
		t.Errorf("symbols %q, want %q", rec.Symbols, want)
	}
	if want := "Foo.xs:N:N: error: implicit declaration of function ‘Perl_foo’ [-Werror=implicit-function-declaration]"; rec.Signature != want {
		t.Errorf("signature %q, want %q", rec.Signature, want)
	}
}

func TestFailureRecordOfDeclined(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"configure.log": "Checking things\nThis module needs a frobnicator.\n"})
	res := &result{Dist: "Foo", Outcome: outcomeDeclined, Phases: []phaseRecord{{Phase: "unpack"}, {Phase: "configure"}, {Phase: "declined"}}}
	rec := failureRecord(dir, res, nil)
	if rec.Phase != "configure" || rec.Cause != causeDeclined {
		t.Errorf("phase %q cause %s; want configure, %s", rec.Phase, rec.Cause, causeDeclined)
	}
	if want := []string{"Checking things", "This module needs a frobnicator."}; !reflect.DeepEqual(rec.Evidence, want) {
		t.Errorf("evidence %q, want %q", rec.Evidence, want)
	}
}

func TestFailureRecordOfTimeOut(t *testing.T) {
	stuckLines := strings.Join([]string{
		`{"pid":10,"ppid":1,"state":"S","elapsed_seconds":300,"cpu_percent":0,"wchan":"do_wait","args":["/usr/bin/timeout","--kill-after=10","300","make","test"],"fds":{},"thread_wchans":{}}`,
		`{"pid":11,"ppid":10,"state":"S","elapsed_seconds":299,"cpu_percent":0,"wchan":"do_wait","args":["make","test"],"fds":{},"thread_wchans":{}}`,
		`{"pid":12,"ppid":11,"state":"S","elapsed_seconds":299,"cpu_percent":0,"wchan":"do_select","args":["/usr/bin/Xvfb",":99"],"fds":{},"thread_wchans":{}}`,
		`{"pid":13,"ppid":11,"state":"R","elapsed_seconds":200,"cpu_percent":95,"wchan":"0","args":["/opt/gperl/bin/gperl","run","t/spin.t"],"fds":{},"thread_wchans":{"0":1}}`,
		`{"pid":14,"ppid":1,"state":"S","elapsed_seconds":300,"cpu_percent":50,"wchan":"0","args":["bash","watch.sh"],"fds":{},"thread_wchans":{}}`,
	}, "\n")
	for _, tc := range []struct {
		name      string
		stuck     string
		cause     cause
		signature string
	}{
		{"spinning", stuckLines, causeTimeLimitSpinning, "gperl waits in running"},
		{"blocked", strings.Replace(stuckLines, `"cpu_percent":95`, `"cpu_percent":1`, 1), causeTimeLimitBlocked, "gperl waits in running"},
		{"no record", "", causeTimeLimit, "t/spin.t .."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{"test.log": "t/spin.t .. \n"}
			if tc.stuck != "" {
				files["test.stuck.jsonl"] = tc.stuck
			}
			writeFiles(t, dir, files)
			res := &result{Dist: "Foo", Outcome: outcomeTimeout, Phases: []phaseRecord{{Phase: "build"}, {Phase: "test", Status: 124, TimedOut: true}}}
			rec := failureRecord(dir, res, nil)
			if rec.Cause != tc.cause || rec.Signature != tc.signature || !rec.TimedOut {
				t.Errorf("cause %s signature %q timed out %v; want %s, %q, true", rec.Cause, rec.Signature, rec.TimedOut, tc.cause, tc.signature)
			}
			if tc.stuck != "" && (len(rec.Stuck.Processes) != 1 || rec.Stuck.Processes[0].Args[2] != "t/spin.t") {
				t.Errorf("stuck processes %+v; want the test program alone", rec.Stuck.Processes)
			}
		})
	}
}

func TestFailureRecordsOfBlocked(t *testing.T) {
	latest := []*result{
		{Dist: "A", Outcome: outcomeBuildFailed, Phases: []phaseRecord{{Phase: "build", Status: 2}}},
		{Dist: "B", Outcome: outcomeBlocked, BlockedBy: []string{"A"}, OwnOutcome: outcomeFail},
		{Dist: "C", Outcome: outcomeBlocked, BlockedBy: []string{"B", "D"}, OwnOutcome: outcomeFail},
		{Dist: "D", Outcome: outcomeConfigureFailed, Phases: []phaseRecord{{Phase: "configure", Status: 1}}},
		{Dist: "E", Outcome: outcomePass},
		// Blocked by each other, run by no one.
		{Dist: "X", Outcome: outcomeBlocked, BlockedBy: []string{"Y"}},
		{Dist: "Y", Outcome: outcomeBlocked, BlockedBy: []string{"X"}},
	}
	records := failureRecords(t.TempDir(), latest, nil)
	got := map[string][]string{}
	for _, r := range records {
		if r.Dist == "E" {
			t.Errorf("a passing distribution has a failure record")
		}
		if r.Outcome == outcomeBlocked {
			if r.Cause != causeBlocked {
				t.Errorf("%s: cause %s, want %s", r.Dist, r.Cause, causeBlocked)
			}
			got[r.Dist] = r.RootBlockers
		}
	}
	want := map[string][]string{"B": {"A"}, "C": {"A", "D"}, "X": {}, "Y": {}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("root blockers %v, want %v", got, want)
	}
}
