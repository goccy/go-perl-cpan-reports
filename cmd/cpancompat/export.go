package main

// The results committed to the repository: what the site is generated
// from. Each run is a directory of its own below the results directory:
//
//	snapshot.json          the CPAN snapshot and the reference set's size
//	reference.jsonl.gz     the reference set: one distribution per line
//	<run>/meta.json        what was measured and how (runMeta)
//	<run>/results.jsonl.gz the last result of every distribution
//	<run>/failures.jsonl.gz the failure record of every one that did not pass
//	<run>/logs.jsonl.gz    the tail of the failing phase's log of each
//
// A run's records are other programs' output in places (the evidence lines,
// the logs): they are kept as text and escaped wherever they are shown.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	publishedSnapshotFile = "snapshot.json"
	referenceSetFile      = "reference.jsonl.gz"
	metaFile              = "meta.json"
	resultsGzFile         = "results.jsonl.gz"
	failuresGzFile        = "failures.jsonl.gz"
	logsGzFile            = "logs.jsonl.gz"
)

// networkPolicy is what the containers of a run reach, as the harness
// arranges it (netguard.sh, dist.sh).
const networkPolicy = "The phases up to the build reach the internet, but not private, link-local or " +
	"shared address space, the machine that runs the containers or its host; DNS goes only to the " +
	"resolver Docker gives the containers. The test phase runs with no network but loopback."

// publishedSnapshot is snapshot.json of a results directory: the CPAN
// snapshot every run of the directory was measured on, and its reference
// set.
type publishedSnapshot struct {
	snapshot
	// Distributions counts the distributions of the snapshot (one latest
	// release each).
	Distributions int `json:"distributions"`
	// Perl is the perl version whose CPAN Testers reports define the
	// reference set.
	Perl string `json:"perl"`
	// ReferenceDistributions is the size of the reference set.
	ReferenceDistributions int `json:"reference_distributions"`
}

// interpreterMeta names the interpreter a run measured.
type interpreterMeta struct {
	// Name is go-perl or perl.
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Commit is the version control revision it was built from.
	Commit string `json:"commit,omitempty"`
}

// runMeta is meta.json of a published run: what was measured and how.
type runMeta struct {
	Run         string          `json:"run"`
	Interpreter interpreterMeta `json:"interpreter"`
	// BaseImage is the image the survey image was built on.
	BaseImage string `json:"base_image,omitempty"`
	// Perl is the perl version whose CPAN Testers reports define the
	// reference set, and PackagesSHA256 identifies the CPAN snapshot.
	Perl           string `json:"perl"`
	PackagesSHA256 string `json:"packages_sha256"`
	// Provisional: the run is not the measurement the results stand for
	// in the end; Note says what it is.
	Provisional bool   `json:"provisional"`
	Note        string `json:"note,omitempty"`
	// ComparedWith names the run of perl itself this run is compared with.
	ComparedWith string `json:"compared_with,omitempty"`
	// FirstResult and LastResult are when the first and the last of the
	// distributions finished; Recorded counts the distributions.
	FirstResult string `json:"first_result"`
	LastResult  string `json:"last_result"`
	Recorded    int    `json:"recorded"`
	// Invocations are the settings of each invocation of cpancompat run
	// that made the run.
	Invocations []invocation `json:"invocations"`
	Network     string       `json:"network"`
}

// logExcerpt is the end of the log of a distribution's failing phase.
type logExcerpt struct {
	Dist string `json:"dist"`
	// Log is the name of the log (test.log, build.log, ...); Bytes is its
	// whole size and Text its last lines.
	Log   string `json:"log"`
	Bytes int64  `json:"bytes"`
	Text  string `json:"text"`
}

const (
	// An excerpt is the last excerptLines lines of a log, at most
	// excerptBytes bytes of them.
	excerptLines = 100
	excerptBytes = 8 << 10
	// excerptWindow is how much of the end of a log is read for it.
	excerptWindow = 64 << 10
)

// exportOptions are the descriptions of a run that its records do not
// hold, given on the command line.
type exportOptions struct {
	dataDir, run, results string
	interpreter           interpreterMeta
	baseImage             string
	provisional           bool
	note                  string
	comparedWith          string
	// invocations is a file holding the invocations of a run made before
	// run recorded them (a JSON array); "" to read invocations.jsonl.
	invocations string
}

// exportReferenceSet writes the snapshot and the reference set of dataDir's
// graph into the results directory.
func exportReferenceSet(dataDir, results string) error {
	dists, err := readDists(filepath.Join(dataDir, distsFile))
	if err != nil {
		return err
	}
	ps, err := snapshotOf(dataDir, dists)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(results, 0o755); err != nil {
		return err
	}
	if err := writeGzipJSONLines(filepath.Join(results, referenceSetFile), referenceSet(dists)); err != nil {
		return err
	}
	return writeJSON(filepath.Join(results, publishedSnapshotFile), ps)
}

// snapshotOf describes the snapshot and reference set of a data directory.
func snapshotOf(dataDir string, dists []dist) (publishedSnapshot, error) {
	var ps publishedSnapshot
	raw, err := os.ReadFile(filepath.Join(dataDir, snapshotFile))
	if err != nil {
		return ps, err
	}
	if err := json.Unmarshal(raw, &ps.snapshot); err != nil {
		return ps, fmt.Errorf("%s: %w", snapshotFile, err)
	}
	if ps.Perl, err = graphPerl(dataDir); err != nil {
		return ps, err
	}
	if ps.Perl == "" {
		return ps, fmt.Errorf("the graph of %s carries no CPAN Testers reports of a perl version (build it with graph -perl)", dataDir)
	}
	ps.Distributions = len(dists)
	ps.ReferenceDistributions = len(referenceSet(dists))
	return ps, nil
}

// exportRun writes a run of a data directory into the results directory,
// which must hold the snapshot and reference set of the same snapshot
// (exportReferenceSet).
func exportRun(opts exportOptions) error {
	if opts.interpreter.Name == "" {
		return fmt.Errorf("export: no interpreter name (-interpreter-name)")
	}
	dists, err := readDists(filepath.Join(opts.dataDir, distsFile))
	if err != nil {
		return err
	}
	ps, err := snapshotOf(opts.dataDir, dists)
	if err != nil {
		return err
	}
	published, err := readPublishedSnapshot(opts.results)
	if err != nil {
		return fmt.Errorf("%w (write the reference set first: cpancompat refset)", err)
	}
	set, err := readGzipJSONLines[refDist](filepath.Join(opts.results, referenceSetFile))
	if err != nil {
		return err
	}
	if ps.PackagesSHA256 != published.PackagesSHA256 || ps.Perl != published.Perl || !sameDists(set, referenceSet(dists)) {
		return fmt.Errorf("export: %s was measured on another snapshot or reference set than the one in %s", opts.dataDir, opts.results)
	}

	rd, err := loadRun(opts.dataDir, opts.run, dists)
	if err != nil {
		return err
	}
	invocations, err := readInvocations(opts)
	if err != nil {
		return err
	}
	red, err := newRedactor(opts.dataDir)
	if err != nil {
		return err
	}
	latest := make([]*result, 0, len(rd.latest))
	for _, res := range rd.latest {
		latest = append(latest, res)
	}
	sort.Slice(latest, func(i, j int) bool { return latest[i].Dist < latest[j].Dist })
	failures := sortedFailures(rd)
	var logs []logExcerpt
	work := filepath.Join(opts.dataDir, "runs", opts.run, "work")
	for i := range failures {
		red.failure(&failures[i])
		if ex, ok := excerptOf(filepath.Join(work, failures[i].Dist), &failures[i]); ok {
			ex.Text = red.text(ex.Text)
			logs = append(logs, ex)
		}
	}

	meta := runMeta{
		Run: opts.run, Interpreter: opts.interpreter, BaseImage: opts.baseImage,
		Perl: ps.Perl, PackagesSHA256: ps.PackagesSHA256,
		Provisional: opts.provisional, Note: opts.note, ComparedWith: opts.comparedWith,
		Recorded: len(latest), Invocations: invocations, Network: networkPolicy,
	}
	for _, res := range latest {
		if meta.FirstResult == "" || res.Finished < meta.FirstResult {
			meta.FirstResult = res.Finished
		}
		if res.Finished > meta.LastResult {
			meta.LastResult = res.Finished
		}
	}

	dir := filepath.Join(opts.results, opts.run)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeGzipJSONLines(filepath.Join(dir, resultsGzFile), latest); err != nil {
		return err
	}
	if err := writeGzipJSONLines(filepath.Join(dir, failuresGzFile), failures); err != nil {
		return err
	}
	if err := writeGzipJSONLines(filepath.Join(dir, logsGzFile), logs); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, metaFile), meta)
}

// sameDists reports whether two reference sets name the same releases.
func sameDists(a, b []refDist) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Dist != b[i].Dist || a[i].Release != b[i].Release {
			return false
		}
	}
	return true
}

// readInvocations returns the invocations of the run: from the file given
// with -invocations, or else the run's invocations.jsonl.
func readInvocations(opts exportOptions) ([]invocation, error) {
	if opts.invocations != "" {
		raw, err := os.ReadFile(opts.invocations)
		if err != nil {
			return nil, err
		}
		var invs []invocation
		if err := json.Unmarshal(raw, &invs); err != nil {
			return nil, fmt.Errorf("%s: %w", opts.invocations, err)
		}
		if len(invs) == 0 {
			return nil, fmt.Errorf("%s: no invocations", opts.invocations)
		}
		return invs, nil
	}
	name := filepath.Join(opts.dataDir, "runs", opts.run, "invocations.jsonl")
	invs, err := readJSONLines[invocation](name)
	if err != nil {
		return nil, err
	}
	if len(invs) == 0 {
		return nil, fmt.Errorf("%s: no invocations recorded (a run made before run recorded them needs -invocations)", name)
	}
	return invs, nil
}

// excerptOf returns the end of the log of the failing phase of f, read from
// dir: the phase's log, or what docker printed for a distribution with no
// failing phase. It reports false when there is no such log or it is
// empty.
func excerptOf(dir string, f *failure) (logExcerpt, bool) {
	var names []string
	if f.Phase != "" {
		names = append(names, f.Phase+".log")
	} else if f.Outcome != outcomeBlocked {
		names = append(names, "test.log", "container.log")
	}
	for _, name := range names {
		text, size, err := logTailText(filepath.Join(dir, name))
		if err != nil || size == 0 {
			continue
		}
		return logExcerpt{Dist: f.Dist, Log: name, Bytes: size, Text: text}, true
	}
	return logExcerpt{}, false
}

// logTailText returns the last lines of a log (excerptLines, at most
// excerptBytes) and the log's size. Only the end of the file is read.
func logTailText(name string) (string, int64, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s: not a regular file", name)
	}
	size := st.Size()
	start := max(0, size-excerptWindow)
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return "", 0, err
	}
	if start > 0 {
		// The first line is cut: it starts after the first newline.
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	return tailLines(strings.ToValidUTF8(string(buf), "�"), excerptLines, excerptBytes), size, nil
}

// tailLines returns the last lines of text, at most n of them and at most
// limit bytes; a last line longer than limit is cut to its end.
func tailLines(text string, n, limit int) string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return ""
	}
	end := len(text)
	start := end
	for lines := 0; lines < n && start > 0; lines++ {
		i := strings.LastIndexByte(text[:start-1], '\n')
		next := i + 1
		if end-next > limit {
			break
		}
		start = next
		if i < 0 {
			break
		}
	}
	if start == end {
		// One line longer than the limit: its end, from a character
		// boundary.
		start = end - limit
		for start < end && !utf8.RuneStart(text[start]) {
			start++
		}
	}
	return text[start:end]
}

// redactor removes what identifies the machine a run was made on from the
// text of its records: the path of its data directory, and paths below the
// user's home directory. The latter reach the logs from the interpreter
// binary itself when it was built on that machine (the source paths of Go
// stack traces, the paths of the toolchain in its %Config): each is cut to
// its last two components.
type redactor struct {
	data *strings.Replacer
	// home matches a path below the home directory, up to the characters
	// that end a path in the messages that carry them (white space,
	// quotes, brackets, and the colon before a line number).
	home    *regexp.Regexp
	homeDir string
}

func newRedactor(dataDir string) (*redactor, error) {
	var pairs []string
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
		pairs = append(pairs, real, "<data>")
	}
	pairs = append(pairs, abs, "<data>")
	r := &redactor{data: strings.NewReplacer(pairs...)}
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		r.homeDir = home
		r.home = regexp.MustCompile(regexp.QuoteMeta(home) + `(?:/[^\s'"<>()\[\]{},;:=]*)?`)
	}
	return r, nil
}

func (r *redactor) text(s string) string {
	s = r.data.Replace(s)
	if r.home == nil {
		return s
	}
	return r.home.ReplaceAllStringFunc(s, func(path string) string {
		rest := strings.Trim(path[len(r.homeDir):], "/")
		if rest == "" {
			return "<home>"
		}
		parts := strings.Split(rest, "/")
		return "<home>/…/" + strings.Join(parts[max(0, len(parts)-2):], "/")
	})
}

// failure redacts the text of a failure record.
func (r *redactor) failure(f *failure) {
	for i := range f.Evidence {
		f.Evidence[i] = r.text(f.Evidence[i])
	}
	for i := range f.Symbols {
		f.Symbols[i] = r.text(f.Symbols[i])
	}
	f.Signature = r.text(f.Signature)
	if f.Stuck != nil {
		for i := range f.Stuck.Processes {
			p := &f.Stuck.Processes[i]
			for j := range p.Args {
				p.Args[j] = r.text(p.Args[j])
			}
		}
	}
}

// writeGzipJSONLines writes rows, one JSON value per line, gzip-compressed;
// the same rows always make the same bytes.
func writeGzipJSONLines[T any](name string, rows []T) error {
	f, err := os.Create(name + ".tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	zw, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(zw, 1<<20)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), name)
}

// readGzipJSONLines reads a file written by writeGzipJSONLines.
func readGzipJSONLines[T any](name string) ([]T, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var rows []T
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var row T
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return rows, nil
}

func readPublishedSnapshot(results string) (publishedSnapshot, error) {
	var ps publishedSnapshot
	raw, err := os.ReadFile(filepath.Join(results, publishedSnapshotFile))
	if err != nil {
		return ps, err
	}
	if err := json.Unmarshal(raw, &ps); err != nil {
		return ps, fmt.Errorf("%s: %w", publishedSnapshotFile, err)
	}
	return ps, nil
}

// publishedRun is one run of a results directory.
type publishedRun struct {
	meta runMeta
	data *runData
	logs map[string]*logExcerpt
}

// published is a results directory, read.
type published struct {
	snapshot publishedSnapshot
	set      []refDist
	runs     map[string]*publishedRun
}

// readPublished reads a results directory: its snapshot, its reference set
// and every run (every directory with a meta.json).
func readPublished(results string) (*published, error) {
	p := &published{runs: map[string]*publishedRun{}}
	var err error
	if p.snapshot, err = readPublishedSnapshot(results); err != nil {
		return nil, err
	}
	if p.set, err = readGzipJSONLines[refDist](filepath.Join(results, referenceSetFile)); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(results)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(results, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, metaFile))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		pr := &publishedRun{logs: map[string]*logExcerpt{}}
		if err := json.Unmarshal(raw, &pr.meta); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(dir, metaFile), err)
		}
		if pr.meta.Run != e.Name() {
			return nil, fmt.Errorf("%s: names run %q", filepath.Join(dir, metaFile), pr.meta.Run)
		}
		if pr.meta.PackagesSHA256 != p.snapshot.PackagesSHA256 || pr.meta.Perl != p.snapshot.Perl {
			return nil, fmt.Errorf("run %s was measured on another snapshot than %s describes", e.Name(), publishedSnapshotFile)
		}
		results, err := readGzipJSONLines[result](filepath.Join(dir, resultsGzFile))
		if err != nil {
			return nil, err
		}
		failures, err := readGzipJSONLines[failure](filepath.Join(dir, failuresGzFile))
		if err != nil {
			return nil, err
		}
		logs, err := readGzipJSONLines[logExcerpt](filepath.Join(dir, logsGzFile))
		if err != nil {
			return nil, err
		}
		latest := make([]*result, len(results))
		for i := range results {
			latest[i] = &results[i]
		}
		pr.data = newRunData(e.Name(), latest, failures)
		for i := range logs {
			pr.logs[logs[i].Dist] = &logs[i]
		}
		p.runs[e.Name()] = pr
	}
	for name, pr := range p.runs {
		if c := pr.meta.ComparedWith; c != "" && p.runs[c] == nil {
			return nil, fmt.Errorf("run %s is compared with run %s, which is not there", name, c)
		}
	}
	return p, nil
}
