package main

// The static site generated from a results directory: an overview, every
// distribution of the reference set (filtered in the browser), a page per
// distribution and the methodology. Every page links to the others by
// relative paths, so the site works below any base path (GitHub Pages
// serves it below /<repository>/). Nothing is loaded from elsewhere. The
// distribution table reads dists.json, which browsers refuse to pages
// opened from the file system: site -serve serves the directory.
//
// Records hold other programs' output (evidence lines, logs, test file
// names): html/template escapes every value for the context it lands in,
// and the distribution table puts its values in the page as text.

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed site
var siteFiles embed.FS

// outcomes lists the outcomes in the order they are presented, with what
// each means.
var outcomes = []string{
	outcomePass, outcomeFail, outcomeBlocked, outcomeTimeout, outcomeBuildFailed,
	outcomeConfigureFailed, outcomeUnmet, outcomeDeclined, outcomeOutputLimit,
	outcomeNoBuildScript, outcomeUnpackFailed, outcomeDriverFailed,
}

var outcomeTitles = map[string]string{
	outcomePass:            "its tests passed",
	outcomeFail:            "it built, and its tests failed",
	outcomeBlocked:         "it did not pass, and a distribution providing a module it lacked did not install",
	outcomeTimeout:         "a phase ran out of time",
	outcomeBuildFailed:     "make or ./Build failed",
	outcomeConfigureFailed: "Makefile.PL or Build.PL failed without writing its build script",
	outcomeUnmet:           "it did not pass, and a requirement it computed cannot be met",
	outcomeDeclined:        "Makefile.PL or Build.PL declined the platform: it ended without error and without a build script",
	outcomeOutputLimit:     "a phase wrote more than 256 MiB and was stopped",
	outcomeNoBuildScript:   "it has neither Makefile.PL nor Build.PL",
	outcomeUnpackFailed:    "its archive could not be unpacked",
	outcomeDriverFailed:    "the harness could not record a result",
}

// siteRun is one run as the site shows it.
type siteRun struct {
	Name  string
	Meta  runMeta
	Label string
	data  *runData
	logs  map[string]*logExcerpt
	// Coverage is how the run went on the reference set.
	Coverage coverage
	// blocks lists, for each distribution, the ones that name it as
	// blocking them.
	blocks map[string][]string
}

// siteData is everything the pages are made from.
type siteData struct {
	Snapshot publishedSnapshot
	// Subject is the run of the interpreter under survey the site is
	// about, Reference the run of perl itself it is compared with (nil
	// when it has none); Runs are all runs, the latest first.
	Subject    *siteRun
	Reference  *siteRun
	Runs       []*siteRun
	Comparison *comparison
	Causes     []causeCount
	// ForkLimitation is the known limitation of fork in go-perl as the
	// subject run shows it; nil when the subject is not a go-perl run.
	ForkLimitation *knownLimitation
	set            []refDist
	inSet          map[string]*refDist
	pages          map[string]string
	// known are the entries of known-causes.json, by distribution.
	known map[string]*knownCause
}

// knownLimitation is a known limitation of the subject run's interpreter:
// its cause and the distributions whose failure in the run is put down to
// it, of which InReference are in the reference set.
type knownLimitation struct {
	Cause       cause
	Dists       []string
	InReference int
}

// buildSiteData chooses the runs the site is about: the latest run that is
// compared with a run of perl (by its last result), or the latest run when
// none is.
func buildSiteData(p *published) (*siteData, error) {
	known, err := knownCauses()
	if err != nil {
		return nil, err
	}
	s := &siteData{Snapshot: p.snapshot, set: p.set, inSet: map[string]*refDist{}, known: known}
	for i := range p.set {
		s.inSet[p.set[i].Dist] = &p.set[i]
	}
	byName := map[string]*siteRun{}
	for name, pr := range p.runs {
		r := &siteRun{Name: name, Meta: pr.meta, Label: pr.meta.Interpreter.Name, data: pr.data, logs: pr.logs, blocks: map[string][]string{}}
		r.Coverage = coverageOn(p.set, pr.data)
		for _, res := range pr.data.latest {
			for _, b := range res.BlockedBy {
				r.blocks[b] = append(r.blocks[b], res.Dist)
			}
		}
		for _, ds := range r.blocks {
			sort.Strings(ds)
		}
		byName[name] = r
		s.Runs = append(s.Runs, r)
	}
	if len(s.Runs) == 0 {
		return nil, fmt.Errorf("site: the results directory has no runs")
	}
	sort.Slice(s.Runs, func(i, j int) bool {
		a, b := s.Runs[i], s.Runs[j]
		if a.Meta.LastResult != b.Meta.LastResult {
			return a.Meta.LastResult > b.Meta.LastResult
		}
		return a.Name < b.Name
	})
	for _, r := range s.Runs {
		if r.Meta.ComparedWith != "" {
			s.Subject, s.Reference = r, byName[r.Meta.ComparedWith]
			break
		}
	}
	if s.Subject == nil {
		s.Subject = s.Runs[0]
	}
	var refData *runData
	if s.Reference != nil {
		refData = s.Reference.data
		c := compareRuns(p.set, s.Subject.data, refData)
		s.Comparison = &c
	}
	s.Causes = causeCounts(p.set, s.Subject.data, refData)
	if s.Subject.Meta.Interpreter.Name == interpreterGoPerl {
		l := &knownLimitation{Cause: causeForkSharesNativeMemory}
		for name, f := range s.Subject.data.failures {
			if f.Cause != l.Cause {
				continue
			}
			l.Dists = append(l.Dists, name)
			if s.inSet[name] != nil {
				l.InReference++
			}
		}
		sort.Strings(l.Dists)
		s.ForkLimitation = l
	}

	// A page for every distribution of the reference set and every one a
	// run recorded.
	names := map[string]bool{}
	for _, d := range p.set {
		names[d.Dist] = true
	}
	for _, r := range s.Runs {
		for name := range r.data.latest {
			names[name] = true
		}
	}
	s.pages = pageNames(names)
	return s, nil
}

var plainDistName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// pageNames gives each distribution the name of its page: the
// distribution's own name, unless it has characters other than letters,
// digits, dots, hyphens and underscores, or differs only in case from
// another's (the site must work on a case-insensitive file system); then
// its name with those characters replaced, and part of a hash of the name.
func pageNames(names map[string]bool) map[string]string {
	folded := map[string]int{}
	for name := range names {
		folded[strings.ToLower(name)]++
	}
	pages := make(map[string]string, len(names))
	for name := range names {
		if plainDistName.MatchString(name) && folded[strings.ToLower(name)] == 1 {
			pages[name] = name
			continue
		}
		var b strings.Builder
		for _, c := range name {
			if c < 0x80 && (c == '.' || c == '-' || c == '_' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
				b.WriteRune(c)
			} else {
				b.WriteByte('_')
			}
		}
		sum := sha256.Sum256([]byte(name))
		pages[name] = strings.TrimLeft(b.String(), ".") + "~" + hex.EncodeToString(sum[:5])
	}
	return pages
}

// distTable is dists.json, the data of the distributions page. Each row
// holds, in the order of Columns, a distribution's name, release and
// dependents, and indexes into Outcomes, Causes and Buckets (-1 for none),
// and the name of its page when it differs from the distribution's.
type distTable struct {
	Subject        string          `json:"subject"`
	Reference      string          `json:"reference,omitempty"`
	SubjectLabel   string          `json:"subject_label"`
	ReferenceLabel string          `json:"reference_label,omitempty"`
	Outcomes       []string        `json:"outcomes"`
	Causes         []labelled      `json:"causes"`
	Buckets        []labelled      `json:"buckets"`
	Columns        []string        `json:"columns"`
	Rows           [][]interface{} `json:"rows"`
}

type labelled struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// distTableOf is the data of the distributions page: a row per reference
// distribution.
func distTableOf(s *siteData) distTable {
	t := distTable{
		Subject: s.Subject.Name, SubjectLabel: s.Subject.Label, Outcomes: outcomes,
		Columns: []string{"dist", "release", "dependents", "subject_outcome", "reference_outcome", "cause", "bucket", "page"},
	}
	causeIndex := map[cause]int{}
	for i, c := range causes {
		causeIndex[c] = i
		t.Causes = append(t.Causes, labelled{string(c), causeTitles[c]})
	}
	bucketIndex := map[bucket]int{}
	refLabel := ""
	if s.Reference != nil {
		t.Reference, t.ReferenceLabel, refLabel = s.Reference.Name, s.Reference.Label, s.Reference.Label
		for i, b := range buckets {
			bucketIndex[b] = i
			t.Buckets = append(t.Buckets, labelled{string(b), bucketTitle(b, s.Subject.Label, refLabel)})
		}
	}
	outcomeIndex := map[string]int{}
	for i, o := range outcomes {
		outcomeIndex[o] = i
	}
	index := func(o string) int {
		if i, ok := outcomeIndex[o]; ok {
			return i
		}
		return -1
	}
	for _, d := range s.set {
		so := s.Subject.data.outcome(d.Dist)
		ro, b, c := -1, -1, -1
		if s.Reference != nil {
			r := s.Reference.data.outcome(d.Dist)
			ro = index(r)
			b = bucketIndex[bucketOf(so, r)]
		}
		if f := s.Subject.data.failures[d.Dist]; f != nil {
			c = causeIndex[f.Cause]
		}
		var page interface{}
		if p := s.pages[d.Dist]; p != d.Dist {
			page = p
		}
		t.Rows = append(t.Rows, []interface{}{d.Dist, d.Release, d.DependentsTotal, index(so), ro, c, b, page})
	}
	return t
}

// distView is what the page of one distribution shows.
type distView struct {
	Dist string
	Ref  *refDist
	Runs []distRunView
}

// distRunView is one run's record of a distribution.
type distRunView struct {
	Run     *siteRun
	Result  *result
	Failure *failure
	// Known is the entry of known-causes.json the failure's cause comes
	// from; nil when its cause is the one its log tells.
	Known *knownCause
	Log   *logExcerpt
	// Blocks names the distributions that name this one as blocking them
	// in the run.
	Blocks []string
	// Unmet are the requirements not met when it was configured.
	Unmet []unmetRequirement
	// TestFiles and FailingTests count the test files the harness
	// reported on and the failing ones.
	TestFiles, FailingTests int
}

func (s *siteData) distView(name string) distView {
	v := distView{Dist: name, Ref: s.inSet[name]}
	for _, r := range []*siteRun{s.Subject, s.Reference} {
		if r == nil {
			continue
		}
		rv := distRunView{Run: r, Result: r.data.latest[name], Failure: r.data.failures[name], Log: r.logs[name], Blocks: r.blocks[name]}
		if k := s.known[name]; k != nil && rv.Failure != nil && rv.Failure.Cause == k.Cause {
			rv.Known = k
		}
		if rv.Result != nil {
			rv.Unmet = rv.Result.Unmet
			rv.TestFiles = len(rv.Result.Tests)
			for _, t := range rv.Result.Tests {
				if t.failed() {
					rv.FailingTests++
				}
			}
		}
		v.Runs = append(v.Runs, rv)
	}
	return v
}

// page is what every page template gets.
type page struct {
	Title string
	// Root leads from the page to the root of the site ("" or "../").
	Root string
	// Current names the page in the navigation.
	Current string
	Site    *siteData
	Dist    *distView
}

// generateSite writes the site of the results directory into out.
func generateSite(results, out string) error {
	p, err := readPublished(results)
	if err != nil {
		return err
	}
	s, err := buildSiteData(p)
	if err != nil {
		return err
	}
	tmpl, err := siteTemplates(s)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(out, "d"), 0o755); err != nil {
		return err
	}
	assets, err := fs.Sub(siteFiles, "site/assets")
	if err != nil {
		return err
	}
	if err := os.CopyFS(filepath.Join(out, "assets"), assets); err != nil {
		return err
	}
	for _, pg := range []struct{ file, tmpl, title string }{
		{"index.html", "index", "Overview"},
		{"dists.html", "dists", "Distributions"},
		{"methodology.html", "methodology", "Methodology and reproduction"},
	} {
		if err := renderPage(tmpl[pg.tmpl], filepath.Join(out, pg.file), page{Title: pg.title, Current: pg.tmpl, Site: s}); err != nil {
			return err
		}
	}
	table, err := json.Marshal(distTableOf(s))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "dists.json"), table, 0o644); err != nil {
		return err
	}
	names := make([]string, 0, len(s.pages))
	for name := range s.pages {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := s.distView(name)
		if err := renderPage(tmpl["dist"], filepath.Join(out, "d", s.pages[name]+".html"),
			page{Title: name, Root: "../", Current: "dists", Site: s, Dist: &v}); err != nil {
			return err
		}
	}
	// No processing by Jekyll on GitHub Pages: the files are served as
	// they are.
	return os.WriteFile(filepath.Join(out, ".nojekyll"), nil, 0o644)
}

func renderPage(t *template.Template, name string, data page) error {
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	if err := t.ExecuteTemplate(f, "layout", data); err != nil {
		f.Close()
		return fmt.Errorf("%s: %w", name, err)
	}
	return f.Close()
}

// siteTemplates parses the layout with each page's template.
func siteTemplates(s *siteData) (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"num": func(v any) (string, error) {
			switch n := v.(type) {
			case int:
				return formatCount(int64(n)), nil
			case int64:
				return formatCount(n), nil
			}
			return "", fmt.Errorf("num: %T is not an integer", v)
		},
		"pct":   func(n, of int) string { return strconv.FormatFloat(percent(n, of), 'f', 1, 64) + "%" },
		"share": func(f float64) string { return strconv.FormatFloat(100*f, 'f', 1, 64) + "%" },
		"date":  formatDate,
		"dates": formatDateRange,
		"subjectOnlyOutcomes": func() string {
			var parts []string
			for _, o := range append(append([]string(nil), outcomes...), "") {
				if n := s.Comparison.SubjectOnlyOutcomes[o]; n > 0 {
					name := o
					if o == "" {
						name = "no record"
					}
					parts = append(parts, name+" "+formatCount(int64(n)))
				}
			}
			return strings.Join(parts, ", ")
		},
		"day":          func(t time.Time) string { return t.UTC().Format("2006-01-02") },
		"duration":     formatSeconds,
		"causeTitle":   func(c cause) string { return causeTitles[c] },
		"outcomeTitle": func(o string) string { return outcomeTitles[o] },
		"bucketTitle":  func(b bucket) string { return bucketTitle(b, s.Subject.Label, s.referenceLabel()) },
		"bucketCount":  func(name string) int { return s.Comparison.Buckets[bucket(name)] },
		"page": func(name string) string {
			if p, ok := s.pages[name]; ok {
				return p + ".html"
			}
			return ""
		},
		"outcomes": func() []string { return outcomes },
		"buckets":  func() []bucket { return buckets },
		"causes":   func() []cause { return causes },
		"first": func(n int, xs []string) []string {
			return xs[:min(n, len(xs))]
		},
		"firstRoots": func(n int, xs []rootBlocker) []rootBlocker { return xs[:min(n, len(xs))] },
		"firstGroups": func(n int, xs []distGroup) []distGroup {
			return xs[:min(n, len(xs))]
		},
		"sub":   func(a, b int) int { return a - b },
		"short": func(s string) string { return s[:min(7, len(s))] },
		"join":  strings.Join,
	}
	layout, err := template.New("layout").Funcs(funcs).ParseFS(siteFiles, "site/layout.html")
	if err != nil {
		return nil, err
	}
	out := map[string]*template.Template{}
	for _, name := range []string{"index", "dists", "dist", "methodology"} {
		t, err := layout.Clone()
		if err != nil {
			return nil, err
		}
		if out[name], err = t.ParseFS(siteFiles, "site/"+name+".html"); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Shown returns the runs the site is about: the subject, and the
// reference run when there is one.
func (s *siteData) Shown() []*siteRun {
	if s.Reference == nil {
		return []*siteRun{s.Subject}
	}
	return []*siteRun{s.Subject, s.Reference}
}

func (s *siteData) referenceLabel() string {
	if s.Reference == nil {
		return ""
	}
	return s.Reference.Label
}

// formatCount writes n with thousands separators.
func formatCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// formatDate writes the day of an RFC 3339 time, or the text as it is when
// it is not one.
func formatDate(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2006-01-02")
}

// formatSeconds writes a duration in seconds in hours and minutes, or
// minutes and seconds under an hour, leaving out units that are zero.
func formatSeconds(n int) string {
	if n == 0 {
		return "0 s"
	}
	var parts []string
	if h := n / 3600; h > 0 {
		parts = append(parts, strconv.Itoa(h)+" h")
	}
	if m := n % 3600 / 60; m > 0 {
		parts = append(parts, strconv.Itoa(m)+" min")
	}
	if s := n % 60; s > 0 && n < 3600 {
		parts = append(parts, strconv.Itoa(s)+" s")
	}
	return strings.Join(parts, " ")
}

// formatDateRange writes the days of two RFC 3339 times, once when they
// are the same.
func formatDateRange(first, last string) string {
	a, b := formatDate(first), formatDate(last)
	if a == b {
		return a
	}
	return a + " to " + b
}

// serveSite serves the directory dir at addr until the process ends.
func serveSite(dir, addr string) error {
	fmt.Fprintf(os.Stderr, "serving %s at http://%s/\n", dir, addr)
	return http.ListenAndServe(addr, http.FileServer(http.Dir(dir)))
}
