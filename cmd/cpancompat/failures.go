package main

// Why a distribution did not pass: the failure record of every recorded
// distribution that did not pass, built from its result and the logs its
// container wrote.

import (
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// cause is why a distribution did not pass, as far as its records and the
// output of its failing phase tell.
type cause string

const (
	// causeBlocked: a distribution providing a module it lacked did not
	// install (the outcome blocked).
	causeBlocked cause = "blocked"
	// causeForkChildNativeCode: a known limitation of go-perl, which runs
	// a fork child as a pseudo-process in the same OS process. The memory
	// native modules allocate is not part of the interpreter's memory that
	// the child gets a copy of, so the child cannot use native code: a
	// call into a native module, loading one, or a hook one installed dies
	// with go-perl's message (forkChildNativeCodeMessage). The evidence
	// tells it where the child's message reached the log; where the child's
	// output was discarded, analysis gives it, recorded in
	// known-causes.json (known.go).
	causeForkChildNativeCode cause = "fork-child-native-code"
	// The causes the evidence lines tell, in the order they are tried.
	causeExternalLibrary cause = "external-library"
	causeOtherOS         cause = "other-os"
	causePerlHeader      cause = "perl-header"
	causeCAPI            cause = "c-api"
	causeThreads         cause = "threads"
	causeTLSOrPTY        cause = "tls-or-pty"
	causeModuleNotFound  cause = "module-not-found"
	causeINCUnreadable   cause = "inc-unreadable"
	causeCrash           cause = "crash"
	causeNetwork         cause = "network"
	// causeOutputLimit: a phase was stopped for writing too much.
	causeOutputLimit cause = "output-limit"
	// A phase that ran out of time, by what its processes were doing
	// shortly before the limit (the phase's stuck.jsonl).
	causeTimeLimitBlocked  cause = "time-limit-blocked"
	causeTimeLimitSpinning cause = "time-limit-spinning"
	causeTimeLimitSlow     cause = "time-limit-slow"
	// causeTimeLimit: a phase ran out of time and no record of its
	// processes tells more.
	causeTimeLimit cause = "time-limit"
	// causeDeclined: the configure script ended successfully without
	// writing a build script, for a reason no evidence line tells.
	causeDeclined cause = "declined"
	// causeNotAnalysed: nothing above applies.
	causeNotAnalysed cause = "not-analysed"
)

// causes lists every cause, in the order they are presented.
var causes = []cause{
	causeBlocked, causeExternalLibrary, causeOtherOS, causePerlHeader, causeCAPI,
	causeThreads, causeForkChildNativeCode, causeTLSOrPTY, causeModuleNotFound, causeINCUnreadable, causeCrash,
	causeNetwork, causeOutputLimit, causeTimeLimitBlocked, causeTimeLimitSpinning,
	causeTimeLimitSlow, causeTimeLimit, causeDeclined, causeNotAnalysed,
}

// causeTitles describes each cause in a few words.
var causeTitles = map[cause]string{
	causeBlocked:             "blocked by a distribution that did not install",
	causeForkChildNativeCode: "fork: native modules cannot be used in a forked child (known limitation)",
	causeExternalLibrary:     "an external C library, header or tool is missing",
	causeOtherOS:             "made for another operating system",
	causePerlHeader:          "a perl header is missing",
	causeCAPI:                "C compile or link errors against the perl API",
	causeThreads:             "needs a perl with threads",
	causeTLSOrPTY:            "TLS or a pseudo-terminal",
	causeModuleNotFound:      "a module not found at run time",
	causeINCUnreadable:       "@INC unreadable after dropping privileges",
	causeCrash:               "a crash of native code or of the interpreter",
	causeNetwork:             "network access",
	causeOutputLimit:         "output limit: a phase wrote more than 256 MiB",
	causeTimeLimitBlocked:    "time limit: blocked (waiting, no CPU)",
	causeTimeLimitSpinning:   "time limit: spinning (one program on the CPU for minutes)",
	causeTimeLimitSlow:       "time limit: slow (still going through its test files)",
	causeTimeLimit:           "time limit (not analysed)",
	causeDeclined:            "declined by its own configure script (not analysed)",
	causeNotAnalysed:         "not analysed",
}

// The causes the evidence lines tell, each by the patterns of the
// messages that tell it. The lines are other programs' output (make, the C
// compiler and linker, perl, the test harness, the distribution's own
// scripts): text in formats this program does not define, so they are
// read by pattern. The first cause whose pattern matches is the cause;
// causeOf gives every record a cause even when none matches.
var evidenceCauses = []struct {
	cause   cause
	matches func(text string) bool
}{
	{causeExternalLibrary, func(text string) bool {
		return missingNonPerlHeader(text) || externalLibraryPattern.MatchString(text)
	}},
	{causeOtherOS, regexp.MustCompile(`OS unsupported|No support for OS|only (works|runs|supported) on (Windows|MSWin|Mac|darwin|VMS)|Win32`).MatchString},
	{causePerlHeader, regexp.MustCompile(`fatal error: (EXTERN|perl|XSUB|embed|proto|regcomp|perlio|patchlevel|[a-z_]+)\.h: No such file`).MatchString},
	{causeCAPI, regexp.MustCompile(`implicit declaration of function|undeclared \(first use|unknown type name|has no member named|` +
		`lvalue required|incompatible (pointer )?type|undefined (reference|symbol)|` +
		`too (few|many) arguments to function|conflicting types|incomplete type|not an lvalue|` +
		`request for member|invalid use of|expected .* before`).MatchString},
	{causeThreads, regexp.MustCompile(`not built to support threads|requires? (i)?threads|This Perl not built to support threads`).MatchString},
	// go-perl's own message, though this program reads it like the others:
	// go-perl defines it, but it reaches this program only as a line of a
	// test's output, through the distribution's tests and the harness,
	// which carry nothing but text. It comes before causeTLSOrPTY, whose
	// pattern matches the names of the native modules (Net::SSLeay,
	// IO::Tty) the message names. Where the evidence also shows a crash,
	// it does not apply and a cause below does: a crash is a defect to
	// look into even where a child also used native code.
	{causeForkChildNativeCode, toldForkChildNativeCode},
	{causeTLSOrPTY, regexp.MustCompile(`SSL (accept|connect) attempt failed|SSL_|IO::Socket::SSL|Net::SSLeay|IO::Pty|IO::Tty|\bpty\b|openpty`).MatchString},
	{causeModuleNotFound, regexp.MustCompile(`Can't locate \S+\.pm in @INC`).MatchString},
	{causeINCUnreadable, regexp.MustCompile(`Can't locate .*Permission denied`).MatchString},
	{causeCrash, crashPattern.MatchString},
	{causeNetwork, regexp.MustCompile(`Temporary failure in name resolution|Could not resolve|Network is unreachable|Connection timed out`).MatchString},
}

// forkChildNativeCodeMessage is in every message go-perl dies with when a
// forked child was to run native code, whatever it was to run: an XSUB
// ("Pkg::name: native modules cannot be used in a forked child (lib.so) at
// ... line N."), a library to load ("Can't load 'lib.so' for module Pkg:
// lib.so: native modules cannot be used ..."), a hook ("the op checker of a
// native module: native modules cannot be used ...").
const forkChildNativeCodeMessage = "native modules cannot be used in a forked child"

var (
	// crashPattern: the messages of a crash, of native code or of the
	// interpreter.
	crashPattern           = regexp.MustCompile(`SIGSEGV|Segmentation|panic:|Assertion|runtime error|SIGBUS|SIGABRT`)
	externalLibraryPattern = regexp.MustCompile(`cannot find -l|` +
		`pkg-config.*not found|Package '?[\w.+-]+'? was not found|` +
		`NA: Unable to build|library not found|Could not find .*lib|Alien::|[Yy]ou need to install|` +
		`\w+-config\b.*(not found|can't find|Unable)|Unable to call script: \S+-config|` +
		`Checking for \S+\.h\.\.\.|-devel\b|configure: error:`)
	missingHeader = regexp.MustCompile(`fatal error: ([\w/.+-]+)\.h(?:pp)?: No such file`)
	// perlHeaderPrefixes start the names of the headers of perl itself
	// (perl.h, XSUB.h, perlio.h, perliol.h, ...): a missing one is
	// causePerlHeader, not a missing external library.
	perlHeaderPrefixes = []string{"EXTERN", "perl", "XSUB", "ppport", "embed", "proto", "regcomp", "perlio", "patchlevel"}
)

// toldForkChildNativeCode reports whether text has go-perl's message for a
// forked child that was to run native code, and no crash.
func toldForkChildNativeCode(text string) bool {
	return strings.Contains(text, forkChildNativeCodeMessage) && !crashPattern.MatchString(text)
}

// missingNonPerlHeader reports whether text has the C compiler's message
// for a missing header whose name does not start like a perl header's.
func missingNonPerlHeader(text string) bool {
	for _, m := range missingHeader.FindAllStringSubmatch(text, -1) {
		perl := false
		for _, p := range perlHeaderPrefixes {
			if strings.HasPrefix(m[1], p) {
				perl = true
				break
			}
		}
		if !perl {
			return true
		}
	}
	return false
}

// causeLine returns the first of the evidence lines that tells c by itself;
// "" when none does, or c is not a cause evidence lines tell.
func causeLine(evidence []string, c cause) string {
	for _, ec := range evidenceCauses {
		if ec.cause != c {
			continue
		}
		for _, line := range evidence {
			if ec.matches(line) {
				return line
			}
		}
	}
	return ""
}

// causeOf is the cause the evidence lines of a distribution that did not
// pass tell, given its outcome. It is total: when no pattern matches, the
// cause is causeTimeLimit for a time-out and causeNotAnalysed otherwise.
func causeOf(evidence []string, outcome string) cause {
	if outcome == outcomeOutputLimit {
		return causeOutputLimit
	}
	text := strings.Join(evidence, "\n")
	for _, c := range evidenceCauses {
		if c.matches(text) {
			return c.cause
		}
	}
	if outcome == outcomeTimeout {
		return causeTimeLimit
	}
	return causeNotAnalysed
}

// failure is the record of one distribution that did not pass.
type failure struct {
	Dist        string `json:"dist"`
	Release     string `json:"release"`
	Outcome     string `json:"outcome"`
	OwnOutcome  string `json:"own_outcome,omitempty"`
	Interpreter string `json:"interpreter,omitempty"`
	// DependentsTotal counts the distributions of the snapshot that need
	// this one, directly or indirectly, in the run's graph.
	DependentsTotal int `json:"dependents_total"`
	// Reference: the distribution belongs to the reference set.
	Reference bool `json:"reference,omitempty"`
	// Unmet names the modules whose requirements were not met when the
	// distribution was configured.
	Unmet     []string `json:"unmet,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"`
	// RootBlockers are the distributions at the ends of the chains of
	// blocked distributions that start at this one: the ones that were
	// run and did not install.
	RootBlockers []string `json:"root_blockers,omitempty"`
	// Phase is the phase that failed (configure for a distribution that
	// declined); "" when none did.
	Phase       string `json:"phase,omitempty"`
	PhaseStatus int    `json:"phase_status,omitempty"`
	TimedOut    bool   `json:"timed_out,omitempty"`
	// TestFiles counts the test files the harness reported on, when some
	// of them failed; FailingTests are those (the first 40).
	TestFiles    int        `json:"test_files,omitempty"`
	FailingTests []testFile `json:"failing_tests,omitempty"`
	// Evidence are the lines of the failing phase's log that say what
	// went wrong.
	Evidence []string `json:"evidence,omitempty"`
	// Symbols are the names the compiler, the linker or perl reported
	// missing.
	Symbols []string `json:"symbols,omitempty"`
	// Stuck is what the processes of a phase that ran out of time were
	// doing shortly before the limit.
	Stuck *stuck `json:"stuck,omitempty"`
	Cause cause  `json:"cause"`
	// Signature is the evidence that groups like failures: the first line
	// that is not a summary every failure prints, with paths of build
	// directories and numbers replaced.
	Signature string `json:"signature,omitempty"`
}

// stuck is what the processes of a phase that ran out of time were doing.
type stuck struct {
	// Kind is blocked (waiting, no CPU), spinning (one program on the
	// CPU for minutes) or slow (a program that started recently is on the
	// CPU: still going through the test files).
	Kind      string         `json:"kind"`
	Processes []stuckProcess `json:"processes"`
}

// stuckProcess is one line of a phase's stuck.jsonl (watch.sh), the
// fields kept.
type stuckProcess struct {
	PID            int            `json:"pid,omitempty"`
	PPID           int            `json:"ppid,omitempty"`
	Args           []string       `json:"args"`
	ElapsedSeconds int            `json:"elapsed_seconds"`
	CPUPercent     int            `json:"cpu_percent"`
	State          string         `json:"state"`
	WChan          string         `json:"wchan"`
	ThreadWChans   map[string]int `json:"thread_wchans,omitempty"`
}

// maxLogRead bounds how much of a log is read for evidence.
const maxLogRead = 4_000_000

// failureRecords builds the record of every distribution of latest that did
// not pass, from its result and the logs below workDir/<dist>/. dists gives
// the graph of the run (dependents, reference set membership).
func failureRecords(workDir string, latest []*result, dists map[string]*dist) []failure {
	var records []failure
	for _, res := range latest {
		if res.Outcome == outcomePass {
			continue
		}
		records = append(records, failureRecord(filepath.Join(workDir, res.Dist), res, dists[res.Dist]))
	}
	attributeRootBlockers(records)
	return records
}

// failureRecord builds the record of one distribution that did not pass;
// dir holds its logs, d is its graph record (nil when the graph lacks it).
func failureRecord(dir string, res *result, d *dist) failure {
	rec := failure{
		Dist: res.Dist, Release: res.Release, Outcome: res.Outcome, OwnOutcome: res.OwnOutcome,
		Interpreter: res.Interpreter, BlockedBy: res.BlockedBy,
	}
	if d != nil {
		rec.DependentsTotal = d.DependentsTotal
		rec.Reference = d.passesOnPerl()
	}
	for _, u := range res.Unmet {
		rec.Unmet = append(rec.Unmet, u.Module)
	}
	p := failingPhase(res.Phases)
	if p != nil {
		rec.Phase, rec.PhaseStatus, rec.TimedOut = p.Phase, p.Status, p.TimedOut
	}
	if res.Outcome == outcomeDeclined {
		// The configure script ended without error and without a build
		// script: its reason is in what it printed.
		rec.Phase = "configure"
	}
	logName := rec.Phase
	if logName == "" {
		logName = "test"
	}
	log := readLog(filepath.Join(dir, logName+".log"))
	if log == "" && rec.Phase == "" {
		log = readLog(filepath.Join(dir, "container.log"))
	}
	var bad []testFile
	for _, t := range res.Tests {
		if t.failed() {
			bad = append(bad, t)
		}
	}
	// lines are the evidence lines the cause is read from, ev the ones the
	// record shows: for failing test files, every evidence line the harness
	// printed for them, of which the first 8 are shown.
	var lines, ev []string
	if len(bad) > 0 {
		rec.TestFiles = len(res.Tests)
		rec.FailingTests = bad[:min(40, len(bad))]
		lines = failingFilesEvidence(log, bad)
		n := min(8, len(lines))
		ev = lines[:n:n]
	}
	if len(lines) == 0 {
		ev = evidenceOf(log, 8)
		if len(ev) == 0 || res.Outcome == outcomeDeclined {
			ev = append(ev, logTail(log, 5)...)
			ev = ev[:min(10, len(ev))]
		}
		lines = ev
	}
	rec.Cause = causeOf(lines, res.Outcome)
	ev = withLine(ev, causeLine(lines, rec.Cause))
	if rec.TimedOut {
		s := stuckOf(filepath.Join(dir, rec.Phase+".stuck.jsonl"))
		rec.Stuck = s
		// A forked child that died of the limitation leaves its parent
		// waiting for it: the test file running at the limit printed
		// go-perl's message.
		running := runningFileEvidence(log)
		switch {
		case toldForkChildNativeCode(strings.Join(running, "\n")):
			rec.Cause = causeForkChildNativeCode
			ev = withLine(ev, causeLine(running, rec.Cause))
		case s != nil:
			rec.Cause = stuckCauses[s.Kind]
		}
	}
	rec.Evidence = ev
	if rec.Phase == "configure" || rec.Phase == "build" {
		rec.Symbols = symbolsOf(log)
	} else {
		rec.Symbols = symbolsOf(strings.Join(ev, "\n"))
	}
	if rec.Cause == causeNotAnalysed && res.Outcome == outcomeDeclined {
		rec.Cause = causeDeclined
	}
	if res.Outcome == outcomeBlocked {
		// It was run all the same (OwnOutcome), and the records above are
		// of that run; what it is put down to is what it lacked.
		rec.Cause = causeBlocked
	}
	rec.Signature = signatureOf(ev, rec.Stuck)
	return rec
}

// failed reports whether the harness counted the test file as failing: a
// failed test, a non-zero exit or another problem it reported.
func (t testFile) failed() bool {
	return t.Failed > 0 || t.Exit != 0 || t.Problems
}

// failingPhase returns the phase where a distribution failed: the first that
// did not succeed, except a configure phase that wrote its build script
// (further phases follow it). It returns nil when none failed.
func failingPhase(phases []phaseRecord) *phaseRecord {
	for i := range phases {
		p := &phases[i]
		if p.Status == 0 && !p.TimedOut {
			continue
		}
		if p.Phase == "configure" && i < len(phases)-1 {
			continue
		}
		return p
	}
	return nil
}

var stuckCauses = map[string]cause{
	"blocked":  causeTimeLimitBlocked,
	"spinning": causeTimeLimitSpinning,
	"slow":     causeTimeLimitSlow,
}

// readLog returns the first maxLogRead bytes of a log, with invalid UTF-8
// replaced; "" when it cannot be read. Logs are other programs' output and
// are only ever read as text.
func readLog(name string) string {
	f, err := os.Open(name)
	if err != nil {
		return ""
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxLogRead))
	if err != nil {
		return ""
	}
	return strings.ToValidUTF8(string(raw), "�")
}

// splitLines splits text at every line boundary Python's str.splitlines
// knows (\n, \r\n, \r, \v, \f, \x1c, \x1d, \x1e, \x85, U+2028, U+2029):
// programs that redraw a progress line end it with a carriage return.
func splitLines(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch r {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			lines = append(lines, text[start:i])
			start = i + size
		case '\r':
			lines = append(lines, text[start:i])
			if i+1 < len(text) && text[i+1] == '\n' {
				size = 2
			}
			start = i + size
		}
		i += size
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

// The lines that say something went wrong. Like evidenceCauses, these are
// patterns of other programs' messages; a line that matches none is simply
// not evidence.
var (
	evidencePattern = regexp.MustCompile(`(error:|Error:|undefined (reference|symbol)|No such file or directory|` +
		`Can't locate |not implemented|Unsupported|unsupported|panic|SIGSEGV|` +
		`Segmentation|Assertion|Undefined subroutine|Permission denied|` +
		`^not ok|^#\s+Failed test|^#\s+(got|expected)|Dubious|` +
		` at \S+ line \d+\.?$|Bail out|timed out|Killed|` +
		`OS unsupported|NA: Unable to build|` +
		`Compilation failed|syntax error|Out of memory|runtime error|` +
		regexp.QuoteMeta(forkChildNativeCodeMessage) + `)`)
	// genericPattern: summary lines every failure prints.
	genericPattern = regexp.MustCompile(`^Dubious, test returned|^Failed tests?:|^make(\[\d+\])?: \*\*\*|^Possible precedence`)
	// testResultLine: the line the harness prints for a test file.
	testResultLine = regexp.MustCompile(`^(t|xt|tests?)/\S+ \.+`)
)

// evidenceLine returns line trimmed and cut to 300 characters.
func evidenceLine(line string) string {
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) > 300 {
		line = string([]rune(line)[:300])
	}
	return line
}

// evidenceOf returns the first n distinct evidence lines of text.
func evidenceOf(text string, n int) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range splitLines(text) {
		if !evidencePattern.MatchString(line) {
			continue
		}
		line = evidenceLine(line)
		key := normalLine(line)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, line)
		if len(out) == n {
			break
		}
	}
	return out
}

// withLine returns ev with line added at its end, unless line is "" or ev
// has it already.
func withLine(ev []string, line string) []string {
	if line == "" || slices.Contains(ev, line) {
		return ev
	}
	return append(ev, line)
}

// runningFileEvidence returns the evidence lines of text after the
// harness's last test result line: in a test phase that ran out of time,
// the output of the test file that was running at the limit (and of what
// earlier files left running). Without a result line, the evidence of the
// whole text.
func runningFileEvidence(text string) []string {
	lines := splitLines(text)
	start := 0
	for i, line := range lines {
		if testResultLine.MatchString(line) {
			start = i + 1
		}
	}
	return evidenceOf(strings.Join(lines[start:], "\n"), len(lines))
}

// failingFilesEvidence returns every evidence line of the failing test
// files, where the harness printed their output one after another: the
// lines before each failing file's own result line. nil when none of them
// has a result line in text or their lines have no evidence.
func failingFilesEvidence(text string, files []testFile) []string {
	lines := splitLines(text)
	var out []string
	seen := map[string]bool{}
	for _, f := range files {
		end := -1
		for i, line := range lines {
			if strings.HasPrefix(line, f.File+" ") {
				end = i
				break
			}
		}
		if end < 0 {
			continue
		}
		start := end
		for start > 0 && !testResultLine.MatchString(lines[start-1]) && end-start < 60 {
			start--
		}
		for _, line := range lines[start:min(end+2, len(lines))] {
			if !evidencePattern.MatchString(line) {
				continue
			}
			line = evidenceLine(line)
			if key := normalLine(line); !seen[key] {
				seen[key] = true
				out = append(out, line)
			}
		}
	}
	return out
}

// logTail returns the last n non-blank lines of text, trimmed.
func logTail(text string, n int) []string {
	var out []string
	for _, line := range splitLines(text) {
		if strings.TrimSpace(line) != "" {
			out = append(out, evidenceLine(line))
		}
	}
	return out[max(0, len(out)-n):]
}

var (
	buildDirPath = regexp.MustCompile(`/work/(?:src/[^/\s]+|[^/\s]+-[A-Za-z0-9]{6}/[^/\s]+)/`)
	stdlibPath   = regexp.MustCompile(`/(?:root|home/[^/\s]+)/\.cache/go-perl/stdlib-[0-9a-f]+/`)
	hexNumber    = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	number       = regexp.MustCompile(`\b\d+\b`)
)

// normalLine is line with what differs between like failures replaced: the
// build directory, the interpreter's library cache, numbers.
func normalLine(line string) string {
	line = buildDirPath.ReplaceAllString(line, "")
	line = stdlibPath.ReplaceAllString(line, "stdlib/")
	line = hexNumber.ReplaceAllString(line, "0xN")
	return number.ReplaceAllString(line, "N")
}

// signatureOf is the signature of a failure with the given evidence: the
// first line that is not a generic summary, normalized; for a phase that
// ran out of time, what the processes at the bottom wait in.
func signatureOf(ev []string, s *stuck) string {
	if s != nil {
		set := map[string]bool{}
		for _, p := range s.Processes {
			name := "?"
			if len(p.Args) > 0 {
				name = path.Base(p.Args[0])
			}
			wchan := p.WChan
			if wchan == "" || wchan == "0" {
				// /proc/<pid>/wchan reads 0 for a process that is not
				// waiting.
				wchan = "running"
			}
			set[name+" waits in "+wchan] = true
		}
		parts := make([]string, 0, len(set))
		for k := range set {
			parts = append(parts, k)
		}
		sort.Strings(parts)
		return strings.Join(parts, "; ")
	}
	for _, line := range ev {
		if !genericPattern.MatchString(line) {
			return normalLine(line)
		}
	}
	if len(ev) > 0 {
		return normalLine(ev[0])
	}
	return ""
}

// symbolPattern finds the names compiler, linker and perl messages report
// missing. GCC quotes names with typographic quotes in a UTF-8 locale and
// with apostrophes in others.
var symbolPattern = regexp.MustCompile(`implicit declaration of function ['‘]([^'’]+)['’]|['‘]([^'’]+)['’] undeclared|` +
	`unknown type name ['‘]([^'’]+)['’]|has no member named ['‘]([^'’]+)['’]|` +
	"undefined (?:reference to|symbol:?) [`'‘]?(\\w+)|" +
	`fatal error: ([\w/.+-]+\.h(?:pp)?): No such file|Can't locate (\S+\.pm)|cannot find -l(\S+)`)

// symbolsOf returns the first 20 distinct names text reports missing.
func symbolsOf(text string) []string {
	var found []string
	seen := map[string]bool{}
	for _, m := range symbolPattern.FindAllStringSubmatch(text, -1) {
		for _, g := range m[1:] {
			if g == "" {
				continue
			}
			if !seen[g] {
				seen[g] = true
				found = append(found, g)
			}
			break
		}
		if len(found) == 20 {
			break
		}
	}
	return found
}

// stuckOf reads a phase's stuck.jsonl and tells what its processes were
// doing: the busiest process when one used the CPU, otherwise the
// processes at the bottom of the phase's process tree. It returns nil when
// the file is missing or unreadable, or names no phase process.
func stuckOf(name string) *stuck {
	procs, err := readJSONLines[stuckProcess](name)
	if err != nil || len(procs) == 0 {
		return nil
	}
	var root *stuckProcess
	for i := range procs {
		if len(procs[i].Args) > 0 && procs[i].Args[0] == "/usr/bin/timeout" {
			root = &procs[i]
			break
		}
	}
	if root == nil {
		return nil
	}
	children := map[int][]int{}
	byPID := map[int]*stuckProcess{}
	for i := range procs {
		p := &procs[i]
		byPID[p.PID] = p
		children[p.PPID] = append(children[p.PPID], i)
	}
	// The processes below the phase's timeout, but the test phase's
	// virtual X server, which waits whatever the tests do.
	under := map[int]bool{}
	frontier := []int{root.PID}
	for len(frontier) > 0 {
		pid := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		for _, i := range children[pid] {
			if p := procs[i].PID; !under[p] {
				under[p] = true
				frontier = append(frontier, p)
			}
		}
	}
	var tree []*stuckProcess
	for i := range procs {
		p := &procs[i]
		if !under[p.PID] {
			continue
		}
		if len(p.Args) > 0 {
			if base := path.Base(p.Args[0]); base == "Xvfb" || base == "xauth" {
				continue
			}
		}
		tree = append(tree, p)
	}
	parents := map[int]bool{}
	for _, p := range tree {
		parents[p.PPID] = true
	}
	brief := func(p *stuckProcess) stuckProcess {
		b := *p
		b.PID, b.PPID = 0, 0
		b.Args = p.Args[:min(6, len(p.Args))]
		return b
	}
	var busy *stuckProcess
	for _, p := range tree {
		if p.CPUPercent >= 20 && (busy == nil || p.CPUPercent > busy.CPUPercent) {
			busy = p
		}
	}
	if busy != nil {
		// One test program that has used the CPU for minutes spins (or is
		// a single very slow test); a phase whose current program started
		// recently is still going through its test files.
		kind := "slow"
		if busy.ElapsedSeconds > 120 {
			kind = "spinning"
		}
		return &stuck{Kind: kind, Processes: []stuckProcess{brief(busy)}}
	}
	s := &stuck{Kind: "blocked"}
	for _, p := range tree {
		if !parents[p.PID] {
			s.Processes = append(s.Processes, brief(p))
		}
	}
	return s
}

// attributeRootBlockers sets the root blockers of the blocked records.
func attributeRootBlockers(records []failure) {
	blockedBy := map[string][]string{}
	for _, r := range records {
		if r.Outcome == outcomeBlocked {
			blockedBy[r.Dist] = r.BlockedBy
		}
	}
	for i := range records {
		if records[i].Outcome == outcomeBlocked {
			records[i].RootBlockers = rootBlockers(records[i].Dist, blockedBy)
		}
	}
}

// rootBlockers returns, sorted, the distributions at the ends of the chains
// of blocking that start at name: following blockedBy (the distributions
// each blocked one names) through blocked ones, the first that is not
// blocked itself. A chain that only returns to where it has been ends in
// none.
func rootBlockers(name string, blockedBy map[string][]string) []string {
	seen := map[string]bool{name: true}
	roots := map[string]bool{}
	stack := append([]string(nil), blockedBy[name]...)
	for len(stack) > 0 {
		b := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[b] {
			continue
		}
		seen[b] = true
		if next, blocked := blockedBy[b]; blocked {
			stack = append(stack, next...)
			continue
		}
		roots[b] = true
	}
	out := make([]string, 0, len(roots))
	for r := range roots {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// latestResults returns the last record of each distribution, sorted by
// name: a distribution recorded again (a later run with -redo) counts by
// its last record.
func latestResults(rows []result) []*result {
	latest := map[string]*result{}
	for i := range rows {
		latest[rows[i].Dist] = &rows[i]
	}
	out := make([]*result, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dist < out[j].Dist })
	return out
}

// writeJSON writes v, indented, to name.
func writeJSON(name string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(name, append(raw, '\n'), 0o644)
}
