package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTestRuns stands in for the containers of runTests: the phase record
// (or error) each test run ends with, by phase, and the runs it was asked
// for.
type fakeTestRuns struct {
	t     *testing.T
	ends  map[string]phaseRecord
	fails map[string]bool
	// stuck is what a run that ran out of time leaves as its record.
	stuck string
	runs  []binaryRun
}

func (f *fakeTestRuns) run(name, logName string, r binaryRun) (phaseRecord, error) {
	f.runs = append(f.runs, r)
	if f.fails[r.phase] {
		return phaseRecord{}, errors.New("docker create: daemon not reachable")
	}
	p := f.ends[r.phase]
	p.Phase = r.phase
	if p.TimedOut && r.stuck != "" {
		if err := os.WriteFile(r.stuck, []byte(f.stuck), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	return p, nil
}

// runTests tells the states of the tests from how the test runs end, and
// leaves the record of the program as it is whatever happens to them.
func TestRunTestsStates(t *testing.T) {
	stuckLine := func(pid, ppid, cpu int, args ...string) string {
		raw, err := json.Marshal(stuckProcess{PID: pid, PPID: ppid, CPUPercent: cpu, ElapsedSeconds: 290, State: "S", Args: args})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw) + "\n"
	}
	blocked := stuckLine(7, 1, 0, testBinaryPath, "-j", "1", "t/a.t") + stuckLine(9, 7, 0, "/tmp/go-perl-65534/perl-x/perl", "t/a.t")
	ok, failed, out := phaseRecord{}, phaseRecord{Status: 1}, phaseRecord{Status: 137, TimedOut: true}
	built := []phaseRecord{{Phase: "modules"}, {Phase: "link"}, {Phase: "test_modules"}, {Phase: "test_link"}, {Phase: "run_empty"}}
	for _, tc := range []struct {
		name   string
		phases []phaseRecord
		ends   map[string]phaseRecord
		fails  map[string]bool
		preset *staticTest
		want   *staticTest // nil: not recorded
		runs   []string
	}{
		{name: "known before", preset: &staticTest{State: testNoRecord}, want: &staticTest{State: testNoRecord}},
		{name: "deps failed", phases: []phaseRecord{{Phase: "link"}, {Phase: "test_modules", Status: 1}},
			want: &staticTest{State: testDepsFailed, Files: 2}},
		{name: "link failed", phases: []phaseRecord{{Phase: "link"}, {Phase: "test_modules"}, {Phase: "test_link", Status: 1}},
			want: &staticTest{State: testLinkFailed, Files: 2}},
		{name: "link timed out", phases: []phaseRecord{{Phase: "link"}, {Phase: "test_modules"}, {Phase: "test_link", TimedOut: true}},
			want: &staticTest{State: testLinkFailed, Files: 2}},
		{name: "pass", phases: built, ends: map[string]phaseRecord{"test_empty": ok},
			want: &staticTest{State: testPass, Files: 2}, runs: []string{"test_empty"}},
		{name: "timeout", phases: built, ends: map[string]phaseRecord{"test_empty": out},
			want: &staticTest{State: testTimeout, Files: 2, Stuck: "blocked"}, runs: []string{"test_empty"}},
		{name: "needs environment", phases: built, ends: map[string]phaseRecord{"test_empty": failed, "test_control": ok},
			want: &staticTest{State: testNeedsEnvironment, Files: 2}, runs: []string{"test_empty", "test_control"}},
		{name: "static failed", phases: built, ends: map[string]phaseRecord{"test_empty": failed, "test_control": out, "test_dynamic": ok},
			want: &staticTest{State: testStaticFailed, Files: 2}, runs: []string{"test_empty", "test_control", "test_dynamic"}},
		{name: "differs from make test", phases: built, ends: map[string]phaseRecord{"test_empty": failed, "test_control": failed, "test_dynamic": failed},
			want: &staticTest{State: testDiffersFromMakeTest, Files: 2}, runs: []string{"test_empty", "test_control", "test_dynamic"}},
		{name: "dynamic out of time", phases: built, ends: map[string]phaseRecord{"test_empty": failed, "test_control": failed, "test_dynamic": out},
			want: &staticTest{State: testDiffersFromMakeTest, Files: 2}, runs: []string{"test_empty", "test_control", "test_dynamic"}},
		{name: "docker fails", phases: built, ends: map[string]phaseRecord{"test_empty": failed}, fails: map[string]bool{"test_control": true},
			runs: []string{"test_empty", "test_control"}},
		{name: "no test phases recorded", phases: []phaseRecord{{Phase: "link"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &staticRunner{modVolume: "mod", testLimit: 300 * time.Second, testMemory: "1500m"}
			f := &fakeTestRuns{t: t, ends: tc.ends, fails: tc.fails, stuck: blocked}
			rec := &staticRecord{Dist: "Own", State: staticStatic, Reason: reasonBundlesSystemLibraries, Phases: slices.Clone(tc.phases)}
			job := &staticJob{own: "Own@1", testFiles: []string{"t/a.t", "t/b.t"}, test: tc.preset, input: []byte("input")}
			s.runTests(job, "c", "c-bin", t.TempDir(), rec, f.run)
			if !reflect.DeepEqual(rec.Test, tc.want) {
				t.Errorf("test = %+v, want %+v", rec.Test, tc.want)
			}
			if rec.State != staticStatic || rec.Reason != reasonBundlesSystemLibraries {
				t.Errorf("the program's record changed: %s %s", rec.State, rec.Reason)
			}
			var ran []string
			for _, r := range f.runs {
				ran = append(ran, r.phase)
			}
			if !reflect.DeepEqual(ran, tc.runs) {
				t.Errorf("runs %v, want %v", ran, tc.runs)
			}
			if got := len(rec.Phases) - len(tc.phases); tc.fails == nil && got != len(tc.runs) {
				t.Errorf("%d phases added for %d runs", got, len(tc.runs))
			}
		})
	}
}

// The three test runs differ only in what runs: the test binary alone, the
// test binary in the build image, gperl test on the module directory.
func TestTestRunsConditions(t *testing.T) {
	s := &staticRunner{modVolume: "mod", testLimit: 300 * time.Second, testMemory: "1500m"}
	failed := phaseRecord{Status: 1}
	f := &fakeTestRuns{t: t, ends: map[string]phaseRecord{"test_empty": failed, "test_control": failed, "test_dynamic": failed}}
	rec := &staticRecord{State: staticStatic, Phases: []phaseRecord{{Phase: "test_modules"}, {Phase: "test_link"}}}
	job := &staticJob{own: "Own@1", testFiles: []string{"t/a.t"}, input: []byte("input")}
	s.runTests(job, "c", "c-bin", t.TempDir(), rec, f.run)
	if len(f.runs) != 3 {
		t.Fatalf("runs: %+v", f.runs)
	}
	empty, control, dynamic := f.runs[0], f.runs[1], f.runs[2]
	for _, r := range f.runs {
		if r.limit != 300*time.Second || r.memory != "1500m" || r.pids != testPids || !r.init || !reflect.DeepEqual(r.env, testEnvironment) {
			t.Errorf("%s: limit %v, memory %s, pids %s, init %v, env %v", r.phase, r.limit, r.memory, r.pids, r.init, r.env)
		}
	}
	if !empty.empty || control.empty || dynamic.empty || empty.stuck == "" || control.stuck != "" || dynamic.stuck != "" {
		t.Errorf("where they run: %+v", f.runs)
	}
	if want := []string{testBinaryPath, "-j", "1", "t/a.t"}; !reflect.DeepEqual(empty.argv, want) || !reflect.DeepEqual(control.argv, append([]string{"xvfb-run", "-a"}, want...)) {
		t.Errorf("the binary's command lines: %v, %v", empty.argv, control.argv)
	}
	if !reflect.DeepEqual(dynamic.argv, []string{"xvfb-run", "-a", "bash", "/opt/cpancompat/static.sh", "test-dynamic", "Own@1"}) ||
		string(dynamic.input) != "input" || !strings.HasPrefix(strings.Join(dynamic.mounts, " "), "-v mod:/opt/gperl/mod") {
		t.Errorf("gperl test's run: %+v", dynamic)
	}
}

// The kill at the limit waits for the record of the processes, which is
// taken before the limit while the container runs; a container that ends
// before is neither recorded nor killed.
func TestSuperviseRun(t *testing.T) {
	var mu sync.Mutex
	var events []string
	note := func(e string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}
	killed := make(chan struct{})
	end, timedOut, snapErr, err := superviseRun(20*time.Millisecond, 10*time.Millisecond,
		func() error {
			note("snapshot started")
			// Longer than what is left before the limit.
			time.Sleep(100 * time.Millisecond)
			note("snapshot done")
			return nil
		},
		func() error {
			note("kill")
			close(killed)
			return nil
		},
		func() (containerEnd, error) {
			<-killed
			return containerEnd{ExitCode: 137}, nil
		})
	if err != nil || snapErr != nil || !timedOut || end.ExitCode != 137 {
		t.Errorf("superviseRun = %+v, %v, %v, %v", end, timedOut, snapErr, err)
	}
	if want := []string{"snapshot started", "snapshot done", "kill"}; !reflect.DeepEqual(events, want) {
		t.Errorf("events %v, want %v", events, want)
	}

	events = nil
	end, timedOut, snapErr, err = superviseRun(time.Hour, time.Hour-time.Second,
		func() error { note("snapshot"); return nil },
		func() error { note("kill"); return nil },
		func() (containerEnd, error) { return containerEnd{ExitCode: 1}, nil })
	if err != nil || snapErr != nil || timedOut || end.ExitCode != 1 || events != nil {
		t.Errorf("a container that ends first: %+v, %v, %v, %v, events %v", end, timedOut, snapErr, err, events)
	}
}

// A target whose own tests turn out to need more once it was built (its
// MYMETA.json's test requirements) runs again once those are settled, with
// them in its test closure.
func TestStaticSettleRerunsForTestRequirements(t *testing.T) {
	archives := t.TempDir()
	all := []dist{
		{Dist: "T", Release: "T-1", Path: "A/AU/AUTHOR/T-1.tar.gz", Modules: []string{"T"}},
		{Dist: "Late", Release: "Late-1", Path: "A/AU/AUTHOR/Late-1.tar.gz", Modules: []string{"Late"}},
	}
	writeFiles(t, archives, map[string]string{"archives/A/AU/AUTHOR/T-1.tar.gz": distArchive(t, "T-1/Makefile.PL"),
		"archives/A/AU/AUTHOR/Late-1.tar.gz": distArchive(t, "Late-1/Makefile.PL")})
	s := &staticRunner{
		opts: staticOptions{dataDir: archives, jobs: 1}, dists: map[string]*dist{"T": &all[0], "Late": &all[1]},
		byModule: map[string]string{"T": "T", "Late": "Late"}, versions: map[string]string{}, provided: map[string]string{},
		targets: map[string]bool{"T": true}, previous: map[string]*staticRecord{}, nodes: map[string]*staticNode{},
		waiters: map[string][]*staticNode{}, requirements: map[string]*distRequirements{}, testRequirements: map[string]*distRequirements{},
		verified: map[string]bool{}, testFiles: map[string][]string{"T": {"t/a.t"}}, treesDir: t.TempDir(),
	}
	openRecords(t, s)
	f := &fakeStatic{t: t, s: s, result: map[string]string{}, starts: map[string][]string{}, forced: map[string]bool{},
		seen: map[string]bool{}, jobs: map[string]*staticJob{},
		mymeta: map[string]string{"T": `{"meta-spec":{"version":2},"prereqs":{"test":{"requires":{"Late":"0"}}}}`}}
	s.ensureNode("T")
	if err := s.schedule(f.attempt); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.order, []string{"T", "Late", "T"}) {
		t.Errorf("order %v", f.order)
	}
	if job := f.jobs["T"]; !reflect.DeepEqual(job.closure, []string{"T"}) || !reflect.DeepEqual(job.testClosure, []string{"Late", "T"}) {
		t.Errorf("T's last job: closure %v, test closure %v", job.closure, job.testClosure)
	}
}

// A target waits for what only its tests need, which no container but that
// distribution's own builds; its program carries none of it. When one of
// them was not built, the target's tests do not run, and its program is
// measured all the same.
func TestStaticWaitsForTestRequirements(t *testing.T) {
	archives := t.TempDir()
	mk := func(name string, testNeeds ...string) dist {
		d := dist{Dist: name, Release: name + "-1", Path: "A/AU/AUTHOR/" + name + "-1.tar.gz", Modules: []string{name}}
		for _, n := range testNeeds {
			d.Requires = append(d.Requires, requirement{Module: n, Phase: "test", Relationship: "requires", Dist: n})
		}
		writeFiles(t, archives, map[string]string{"archives/" + d.Path: distArchive(t, d.Release+"/Makefile.PL")})
		return d
	}
	all := []dist{mk("Tested", "TestDep"), mk("TestDep"), mk("Tested2", "TestBroken"), mk("TestBroken"), mk("Untested")}
	s := &staticRunner{
		opts: staticOptions{dataDir: archives, jobs: 1}, dists: map[string]*dist{}, byModule: map[string]string{},
		versions: map[string]string{}, provided: map[string]string{}, targets: map[string]bool{},
		previous: map[string]*staticRecord{}, nodes: map[string]*staticNode{}, waiters: map[string][]*staticNode{},
		requirements: map[string]*distRequirements{}, testRequirements: map[string]*distRequirements{}, verified: map[string]bool{},
		testFiles: map[string][]string{"Tested": {"t/a.t"}, "Tested2": {"t/b.t", "t/c.t"}},
		treesDir:  t.TempDir(),
	}
	for i := range all {
		s.dists[all[i].Dist] = &all[i]
		s.byModule[all[i].Dist] = all[i].Dist
	}
	openRecords(t, s)
	f := &fakeStatic{t: t, s: s, result: map[string]string{"TestBroken": staticDependency + ":" + reasonDistBuildFailed},
		mymeta: map[string]string{}, starts: map[string][]string{}, forced: map[string]bool{}, seen: map[string]bool{}, jobs: map[string]*staticJob{}}
	for _, name := range []string{"Tested", "Tested2", "Untested"} {
		s.targets[name] = true
		s.ensureNode(name)
	}
	if err := s.schedule(f.attempt); err != nil {
		t.Fatal(err)
	}
	if i, j := slices.Index(f.order, "TestDep"), slices.Index(f.order, "Tested"); i < 0 || j < 0 || i > j {
		t.Errorf("TestDep (%d) did not run before Tested (%d): %v", i, j, f.order)
	}
	job := f.jobs["Tested"]
	if !reflect.DeepEqual(job.closure, []string{"Tested"}) || !reflect.DeepEqual(job.testClosure, []string{"TestDep", "Tested"}) || job.test != nil {
		t.Errorf("Tested's job: closure %v, test closure %v, test %+v", job.closure, job.testClosure, job.test)
	}
	if job := f.jobs["Tested2"]; job == nil || !reflect.DeepEqual(job.test, &staticTest{State: testDepsFailed, Files: 2, BlockedBy: []string{"TestBroken"}}) {
		t.Errorf("Tested2's job: %+v", job)
	}
	if r := s.nodes["Tested2"].rec; r.State != staticStatic {
		t.Errorf("Tested2: %+v (what only its tests need does not block its program)", r)
	}
	if job := f.jobs["Untested"]; job == nil || !reflect.DeepEqual(job.test, &staticTest{State: testNoRecord}) {
		t.Errorf("Untested's job: %+v", job)
	}
}

// A target's container gets the snapshot of its test closure, the trees
// only its tests need and its test files.
func TestTestInputInTheAppArchive(t *testing.T) {
	archives := t.TempDir()
	writeFiles(t, archives, map[string]string{"archives/A/AU/AUTHOR/Dep-1.tar.gz": distArchive(t, "Dep-1/Makefile.PL"),
		"archives/A/AU/AUTHOR/Own-1.tar.gz": distArchive(t, "Own-1/Makefile.PL")})
	s := &staticRunner{
		opts: staticOptions{dataDir: archives}, verified: map[string]bool{"Own": true},
		dists: map[string]*dist{
			"Own": {Dist: "Own", Release: "Own-1", Path: "A/AU/AUTHOR/Own-1.tar.gz", Modules: []string{"Own"}},
			"Dep": {Dist: "Dep", Release: "Dep-1", Path: "A/AU/AUTHOR/Dep-1.tar.gz", Modules: []string{"Dep"}},
		},
		versions: map[string]string{"Own": "1", "Dep": "1"}, provided: map[string]string{},
		requirements: map[string]*distRequirements{}, treesDir: t.TempDir(),
		nodes: map[string]*staticNode{"Dep": {rec: &staticRecord{State: staticDependency}}},
	}
	job := &staticJob{closure: []string{"Own"}, testClosure: []string{"Dep", "Own"}, testFiles: []string{"t/a.t", "t/b.t"}}
	in, err := s.testInputOf(job)
	if err != nil || job.test != nil {
		t.Fatalf("testInputOf = %+v, %v; test %+v", in, err, job.test)
	}
	if !reflect.DeepEqual(in.trees, []string{"Dep@1\tA/AU/AUTHOR/Dep-1.tar.gz\t0"}) || !bytes.Contains(in.snapshot, []byte("  Dep-1\n")) {
		t.Errorf("test input: trees %q, snapshot:\n%s", in.trees, in.snapshot)
	}
	archive, err := appArchive([]byte("snapshot"), []string{"Own"}, []string{"Own@1\tA/AU/AUTHOR/Own-1.tar.gz\t1"}, in)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name] = string(data)
	}
	if files["app/test-files"] != "t/a.t\nt/b.t\n" || files["app/test-trees"] != "Dep@1\tA/AU/AUTHOR/Dep-1.tar.gz\t0\n" ||
		files["app/test-cpanfile.snapshot"] != string(in.snapshot) || files["app/cpanfile.snapshot"] != "snapshot" {
		t.Errorf("app archive: %q", files)
	}
}

// The test binary's processes 15 seconds before its limit say whether it
// was blocked or spinning: the ones below the binary, which shares the
// container with its init.
func TestStuckOfATestBinary(t *testing.T) {
	dir := t.TempDir()
	line := func(pid, ppid, cpu, elapsed int, args ...string) string {
		raw, err := json.Marshal(stuckProcess{PID: pid, PPID: ppid, CPUPercent: cpu, ElapsedSeconds: elapsed, State: "S", Args: args})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw) + "\n"
	}
	head := line(1, 0, 0, 300, "/sbin/docker-init", "--", testBinaryPath) + line(7, 1, 0, 300, testBinaryPath, "-j", "1", "t/a.t")
	writeFiles(t, dir, map[string]string{
		"blocked.jsonl":  head + line(9, 7, 0, 290, "/tmp/go-perl-65534/perl-x/perl", "t/a.t"),
		"spinning.jsonl": head + line(9, 7, 99, 290, "/tmp/go-perl-65534/perl-x/perl", "t/a.t"),
	})
	for _, kind := range []string{"blocked", "spinning"} {
		s := stuckOf(filepath.Join(dir, kind+".jsonl"), testBinaryPath)
		if s == nil || s.Kind != kind || len(s.Processes) != 1 || s.Processes[0].Args[1] != "t/a.t" {
			t.Errorf("%s: %+v", kind, s)
		}
	}
	if s := stuckOf(filepath.Join(dir, "blocked.jsonl"), "/usr/bin/timeout"); s != nil {
		t.Errorf("a record without dist.sh's timeout: %+v", s)
	}
}

// A target forced to start before a distribution only its tests need was
// settled does not build that one: its tests do not run, and name it.
func TestForcedTargetTestsWaitForNone(t *testing.T) {
	s := &staticRunner{nodes: map[string]*staticNode{"Member": {}}}
	for _, testClosure := range [][]string{{"Member", "Own"}, {"Unscheduled", "Own"}} {
		job := &staticJob{closure: []string{"Own"}, testClosure: testClosure, testFiles: []string{"t/a.t"}}
		in, err := s.testInputOf(job)
		if err != nil || in != nil || !reflect.DeepEqual(job.test, &staticTest{State: testDepsFailed, Files: 1, BlockedBy: testClosure[:1]}) {
			t.Errorf("%v: input %+v, %v, test %+v", testClosure, in, err, job.test)
		}
	}
}

// A test binary runs with the test limit and memory of the run's latest
// invocation that recorded a limit.
func TestRunTestSettings(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"invocations.jsonl": `{"image":"a","harness":{"module":"m"},"memory":"1500m","test_seconds":300}` + "\n" +
			`{"image":"b","harness":{"module":"m"},"memory":"2g","test_seconds":1200}` + "\n" +
			`{"image":"c","harness":{"module":"m"},"memory":"4g"}` + "\n",
		"none.jsonl":  `{"image":"a","harness":{"module":"m"},"memory":"1g"}` + "\n",
		"nomem.jsonl": `{"image":"a","harness":{"module":"m"},"test_seconds":300}` + "\n",
	})
	if limit, memory, err := runTestSettings(filepath.Join(dir, "invocations.jsonl")); err != nil || limit != 1200*time.Second || memory != "2g" {
		t.Errorf("runTestSettings = %v, %q, %v", limit, memory, err)
	}
	for _, name := range []string{"none.jsonl", "nomem.jsonl"} {
		if _, _, err := runTestSettings(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s: a run without a test limit or memory has no error", name)
		}
	}
}

// The log that tells why a static target's tests did not pass.
func TestStaticLogOfTests(t *testing.T) {
	for state, want := range map[string]string{
		testPass: "", testNoRecord: "", testDepsFailed: "test_modules.log", testLinkFailed: "test_link.log",
		testNeedsEnvironment: "test_empty.log", testStaticFailed: "test_empty.log", testDiffersFromMakeTest: "test_empty.log", testTimeout: "test_empty.log",
	} {
		if got := staticLogOf(&staticRecord{State: staticStatic, Test: &staticTest{State: state}}); got != want {
			t.Errorf("staticLogOf(static, test %s) = %q, want %q", state, got, want)
		}
	}
	ids := map[string]bool{}
	for _, c := range staticTestCategories() {
		if ids[c.ID] || c.Title == "" {
			t.Errorf("category %+v", c)
		}
		ids[c.ID] = true
	}
	if len(ids) != 8 {
		t.Errorf("%d test categories, want the 8 states", len(ids))
	}
}
