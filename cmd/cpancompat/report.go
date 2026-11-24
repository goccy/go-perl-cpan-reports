package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// coverage counts, for one set of distributions, how a run went.
type coverage struct {
	// Distributions is the size of the set.
	Distributions int `json:"distributions"`
	// Recorded is how many of them the run has a result for.
	Recorded int `json:"recorded"`
	// Outcomes counts the recorded ones by outcome.
	Outcomes map[string]int `json:"outcomes"`
	// PassShareOfRecorded and PassShareOfAll are the share of passing
	// distributions among the recorded ones and among the whole set.
	PassShareOfRecorded float64 `json:"pass_share_of_recorded"`
	PassShareOfAll      float64 `json:"pass_share_of_all"`
}

// blocker is a distribution that others were not attempted because of.
type blocker struct {
	Dist    string `json:"dist"`
	Outcome string `json:"outcome"`
	// Blocked counts the recorded distributions that name it as what
	// blocked them; DependentsTotal is everything that needs it.
	Blocked         int `json:"blocked"`
	DependentsTotal int `json:"dependents_total"`
}

// runSummary is what report writes to summary.json.
type runSummary struct {
	Run string `json:"run"`
	// Perl is the perl version the interpreter is built from, whose CPAN
	// Testers reports define Reference: the distributions whose release
	// passes there more often than it fails on Linux (dist.passesOnPerl),
	// the set this survey measures against.
	Perl      string   `json:"perl,omitempty"`
	Reference coverage `json:"reference"`
	// All is every distribution of the snapshot. PassOnPerl are the ones
	// whose CPAN Testers reports on any perl, summed, pass more often than
	// they fail; AnyPassOnPerl the ones with at least one passing report.
	All           coverage  `json:"all"`
	PassOnPerl    coverage  `json:"pass_on_perl"`
	AnyPassOnPerl coverage  `json:"any_pass_on_perl"`
	Blockers      []blocker `json:"blockers"`
	// Causes counts the causes of the run's own failures (not blocked) on
	// the reference set.
	Causes []causeCount `json:"causes"`
	// Comparison is how the run went against a run of perl itself on the
	// reference set (report -reference-data, -reference-run).
	Comparison *comparison `json:"comparison,omitempty"`
}

// referenceRun names a run of perl itself, in the same harness, that a run
// is compared with: its data directory and its name.
type referenceRun struct {
	dataDir, run string
}

// loadRun reads a run of dataDir, a run of interpreter: its last result per
// distribution and the failure records of the ones that did not pass, built
// from its logs, with the known causes applied (applyKnownCauses).
func loadRun(dataDir, run, interpreter string, dists []dist) (*runData, error) {
	known, err := knownCauses()
	if err != nil {
		return nil, err
	}
	rows, err := readJSONLines[result](filepath.Join(dataDir, "runs", run, "results.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("run %s in %s has no results", run, dataDir)
	}
	byName := make(map[string]*dist, len(dists))
	for i := range dists {
		byName[dists[i].Dist] = &dists[i]
	}
	latest := latestResults(rows)
	failures := failureRecords(filepath.Join(dataDir, "runs", run, "work"), latest, byName)
	rd := newRunData(run, latest, failures)
	rd.applyKnownCauses(known, interpreter)
	return rd, nil
}

// report summarizes a run of interpreter: outcomes, coverage against the
// distributions that pass on perl, what blocks the most, why each
// distribution that did not pass failed, and, given a reference run, how the
// two compare. It writes summary.json, failures.jsonl and failures.md into
// the run's directory.
func report(dataDir, run, interpreter string, ref referenceRun, w io.Writer) error {
	dists, err := readDists(filepath.Join(dataDir, "dists.jsonl"))
	if err != nil {
		return err
	}
	perl, err := graphPerl(dataDir)
	if err != nil {
		return err
	}
	rd, err := loadRun(dataDir, run, interpreter, dists)
	if err != nil {
		return err
	}
	runDir := filepath.Join(dataDir, "runs", run)
	failures := sortedFailures(rd)

	s := runSummary{Run: run, Perl: perl}
	sets := []struct {
		c      *coverage
		member func(d *dist) bool
	}{
		{&s.Reference, (*dist).passesOnPerl},
		{&s.All, func(*dist) bool { return true }},
		{&s.PassOnPerl, func(d *dist) bool { return d.Testers != nil && d.Testers.Pass > d.Testers.Fail }},
		{&s.AnyPassOnPerl, func(d *dist) bool { return d.Testers != nil && d.Testers.Pass > 0 }},
	}
	for _, set := range sets {
		set.c.Outcomes = map[string]int{}
	}
	dependents := map[string]int{}
	for i := range dists {
		d := &dists[i]
		dependents[d.Dist] = d.DependentsTotal
		for _, set := range sets {
			if !set.member(d) {
				continue
			}
			set.c.Distributions++
			if res := rd.latest[d.Dist]; res != nil {
				set.c.Recorded++
				set.c.Outcomes[res.Outcome]++
			}
		}
	}
	for _, set := range sets {
		c := set.c
		if c.Recorded > 0 {
			c.PassShareOfRecorded = float64(c.Outcomes[outcomePass]) / float64(c.Recorded)
		}
		if c.Distributions > 0 {
			c.PassShareOfAll = float64(c.Outcomes[outcomePass]) / float64(c.Distributions)
		}
	}

	blocked := map[string]int{}
	for _, res := range rd.latest {
		for _, b := range res.BlockedBy {
			blocked[b]++
		}
	}
	for name, n := range blocked {
		b := blocker{Dist: name, Blocked: n, DependentsTotal: dependents[name]}
		if res := rd.latest[name]; res != nil {
			b.Outcome = res.Outcome
		}
		s.Blockers = append(s.Blockers, b)
	}
	sort.Slice(s.Blockers, func(i, j int) bool {
		a, b := s.Blockers[i], s.Blockers[j]
		if a.DependentsTotal != b.DependentsTotal {
			return a.DependentsTotal > b.DependentsTotal
		}
		return a.Dist < b.Dist
	})

	set := referenceSet(dists)
	var refData *runData
	if ref.run != "" {
		refDists, err := readDists(filepath.Join(ref.dataDir, "dists.jsonl"))
		if err != nil {
			return err
		}
		if refData, err = loadRun(ref.dataDir, ref.run, interpreterPerl, refDists); err != nil {
			return err
		}
		c := compareRuns(set, rd, refData)
		s.Comparison = &c
	}
	s.Causes = causeCounts(set, rd, refData)

	if err := writeJSON(filepath.Join(runDir, "summary.json"), s); err != nil {
		return err
	}
	if err := writeJSONLines(filepath.Join(runDir, "failures.jsonl"), failures); err != nil {
		return err
	}
	if err := writeFailuresMarkdown(filepath.Join(runDir, "failures.md"), run, rd, failures, dependents); err != nil {
		return err
	}

	print := func(title string, c coverage) {
		fmt.Fprintf(w, "%s: %d distributions, %d recorded, %d pass (%.1f%% of recorded, %.1f%% of all)\n",
			title, c.Distributions, c.Recorded, c.Outcomes[outcomePass], 100*c.PassShareOfRecorded, 100*c.PassShareOfAll)
		for _, name := range byCount(c.Outcomes) {
			fmt.Fprintf(w, "  %-22s %6d %5.1f%%\n", name, c.Outcomes[name], percent(c.Outcomes[name], c.Distributions))
		}
		if missing := c.Distributions - c.Recorded; missing > 0 {
			fmt.Fprintf(w, "  %-22s %6d %5.1f%%\n", "(no record)", missing, percent(missing, c.Distributions))
		}
	}
	if perl != "" {
		print(fmt.Sprintf("passing on perl %s on Linux (more passing than failing CPAN Testers reports on the release there; the reference)", perl), s.Reference)
	}
	print("all of CPAN", s.All)
	print("more passing than failing CPAN Testers reports on any perl, summed", s.PassOnPerl)
	print("with at least one passing CPAN Testers report on any perl", s.AnyPassOnPerl)
	if len(s.Blockers) > 0 {
		fmt.Fprintln(w, "not installable, by how much depends on them:")
		for i, b := range s.Blockers {
			if i == 30 {
				break
			}
			fmt.Fprintf(w, "  %-32s %-20s needed by %5d, blocked %4d here\n", b.Dist, b.Outcome, b.DependentsTotal, b.Blocked)
		}
	}
	if len(s.Causes) > 0 {
		fmt.Fprintln(w, "causes of the failures on the reference set (not blocked):")
		for _, c := range s.Causes {
			fmt.Fprintf(w, "  %6d  %s\n", c.Distributions, causeTitles[c.Cause])
		}
	}
	if s.Comparison != nil {
		printComparison(w, s.Comparison)
	}
	return nil
}

// percent is n as a percentage of of (0 for an empty whole).
func percent(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return 100 * float64(n) / float64(of)
}

// sortedFailures returns the failure records of a run sorted by name.
func sortedFailures(r *runData) []failure {
	out := make([]failure, 0, len(r.failures))
	for _, f := range r.failures {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dist < out[j].Dist })
	return out
}

// byCount returns the keys of counts, the largest count first.
func byCount[K ~string](counts map[K]int) []K {
	keys := make([]K, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

// bucketTitle describes a bucket of a comparison, naming the runs by the
// given names.
func bucketTitle(b bucket, subject, reference string) string {
	switch b {
	case bucketBothPass:
		return "both pass"
	case bucketSubjectOnlyFails:
		return subject + " only fails"
	case bucketBothFail:
		return "both fail"
	case bucketReferenceOnlyFails:
		return reference + " only fails"
	}
	return string(b)
}

func printComparison(w io.Writer, c *comparison) {
	fmt.Fprintf(w, "compared with run %s on the %d reference distributions (no record counts as not passing):\n", c.Reference, c.Distributions)
	for _, b := range buckets {
		fmt.Fprintf(w, "  %-28s %6d (%.1f%%)\n", bucketTitle(b, c.Subject, c.Reference), c.Buckets[b], percent(c.Buckets[b], c.Distributions))
	}
	fmt.Fprintf(w, "  %s passes %d, %s passes %d; %s passes %.1f%% of what %s passes\n",
		c.Reference, c.ReferencePasses, c.Subject, c.SubjectPasses, c.Subject, 100*c.ShareOfReferencePasses, c.Reference)
	if c.RecordedByBoth < c.Distributions {
		var parts []string
		for _, b := range buckets {
			parts = append(parts, fmt.Sprintf("%s %d", bucketTitle(b, c.Subject, c.Reference), c.BucketsRecordedByBoth[b]))
		}
		fmt.Fprintf(w, "  recorded by both so far: %d: %s\n", c.RecordedByBoth, strings.Join(parts, ", "))
	}
	var parts []string
	for _, o := range byCount(c.SubjectOnlyOutcomes) {
		parts = append(parts, fmt.Sprintf("%s %d", orNoRecord(o), c.SubjectOnlyOutcomes[o]))
	}
	fmt.Fprintf(w, "%s only fails, by its outcome: %s\n", c.Subject, strings.Join(parts, ", "))
	if len(c.RootBlockers) > 0 {
		fmt.Fprintln(w, "where the chains of blocking end, by how many of those they block (first 30):")
		for i, b := range c.RootBlockers {
			if i == 30 {
				break
			}
			fmt.Fprintf(w, "  %5d %-36s %s %-20s %s %s\n", b.Blocked, b.Dist,
				c.Subject, orNoRecord(b.SubjectOutcome), c.Reference, orNoRecord(b.ReferenceOutcome))
		}
	}
	show := func(title string, groups []distGroup, label func(g distGroup) string) {
		n := 0
		for _, g := range groups {
			n += len(g.Dists)
		}
		fmt.Fprintf(w, "%s: %d distributions (first 40 groups)\n", title, n)
		for i, g := range groups {
			if i == 40 {
				break
			}
			fmt.Fprintf(w, "  %5d  %s\n         %s\n", len(g.Dists), label(g), strings.Join(g.Dists[:min(8, len(g.Dists))], ", "))
		}
	}
	show(c.Subject+" only fails, blocked, by the distribution named as blocking", c.BlockedGroups,
		func(g distGroup) string { return g.Blocker })
	show(c.Subject+" only fails, the others, by outcome, cause and evidence", c.OwnGroups, func(g distGroup) string {
		return fmt.Sprintf("%s; %s; %s", g.Outcome, causeTitles[g.Cause], g.Signature)
	})
}

// orNoRecord is outcome, or a placeholder for a distribution with no record.
func orNoRecord(outcome string) string {
	if outcome == "" {
		return "(no record)"
	}
	return outcome
}

// writeFailuresMarkdown writes the failure records grouped: the
// distributions that block others, the causes, the names missing at
// compile, link or load time, and per cause the distributions by
// signature. dependents gives the dependents of each distribution in the
// run's graph.
func writeFailuresMarkdown(name, run string, r *runData, records []failure, dependents map[string]int) error {
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	byOutcome := map[string]int{}
	for _, res := range r.latest {
		byOutcome[res.Outcome]++
	}
	var outcomes []string
	for _, o := range byCount(byOutcome) {
		outcomes = append(outcomes, fmt.Sprintf("%s %d", o, byOutcome[o]))
	}
	fmt.Fprintf(w, "# Distributions that did not pass: run %s\n\n", run)
	fmt.Fprintf(w, "%d recorded: %s\n\n", len(r.latest), strings.Join(outcomes, ", "))
	fmt.Fprint(w, "Every record is in `failures.jsonl` (one line per distribution: outcome, failing phase, "+
		"failing test files, evidence lines from the log, names found in compiler, linker and loader errors, cause). "+
		"Logs: `work/<distribution>/`. \"In the reference set\" counts the distributions whose CPAN Testers reports "+
		"on the perl version the interpreter is built from pass more often than they fail on Linux.\n\n")

	fmt.Fprint(w, "## Distributions that block others\n\n")
	fmt.Fprint(w, "| distribution that did not install | its outcome | its cause | blocked by it here (directly or through others) | dependents in all of CPAN |\n|---|---|---|---|---|\n")
	roots := map[string]int{}
	for _, rec := range records {
		for _, b := range rec.RootBlockers {
			roots[b]++
		}
	}
	for i, b := range byCount(roots) {
		if i == 200 {
			break
		}
		outcome, c := "not recorded", ""
		if res := r.latest[b]; res != nil {
			outcome = res.Outcome
		}
		if rec := r.failures[b]; rec != nil {
			c = causeTitles[rec.Cause]
		}
		fmt.Fprintf(w, "| %s | %s | %s | %d | %d |\n", b, outcome, c, roots[b], dependents[b])
	}

	groups := map[cause][]failure{}
	counts := map[cause]int{}
	for _, rec := range records {
		if rec.Cause != causeBlocked {
			groups[rec.Cause] = append(groups[rec.Cause], rec)
			counts[rec.Cause]++
		}
	}
	order := byCount(counts)
	fmt.Fprint(w, "\n## Causes\n\n| cause | distributions | of them in the reference set |\n|---|---|---|\n")
	for _, c := range order {
		ref := 0
		for _, rec := range groups[c] {
			if rec.Reference {
				ref++
			}
		}
		fmt.Fprintf(w, "| %s | %d | %d |\n", causeTitles[c], len(groups[c]), ref)
	}

	fmt.Fprint(w, "\n## Names missing at compile, link or load time\n\n| name | distributions | examples |\n|---|---|---|\n")
	symbols := map[string][]string{}
	symbolCount := map[string]int{}
	for _, rec := range records {
		switch rec.Cause {
		case causePerlHeader, causeCAPI, causeModuleNotFound, causeExternalLibrary:
			for _, s := range rec.Symbols {
				symbols[s] = append(symbols[s], rec.Dist)
				symbolCount[s]++
			}
		}
	}
	for i, s := range byCount(symbolCount) {
		if i == 300 {
			break
		}
		ds := symbols[s]
		fmt.Fprintf(w, "| `%s` | %d | %s |\n", s, len(ds), strings.Join(ds[:min(5, len(ds))], ", "))
	}

	for _, c := range order {
		rs := groups[c]
		fmt.Fprintf(w, "\n## %s (%d)\n\n", causeTitles[c], len(rs))
		bySignature := map[string][]failure{}
		sigCount := map[string]int{}
		for _, rec := range rs {
			bySignature[rec.Signature] = append(bySignature[rec.Signature], rec)
			sigCount[rec.Signature]++
		}
		for i, sig := range byCount(sigCount) {
			if i == 400 {
				break
			}
			ds := bySignature[sig]
			sort.SliceStable(ds, func(i, j int) bool { return ds[i].DependentsTotal > ds[j].DependentsTotal })
			var names []string
			for _, d := range ds[:min(12, len(ds))] {
				names = append(names, fmt.Sprintf("%s (%s)", d.Dist, d.Outcome))
			}
			more := ""
			if len(ds) > 12 {
				more = " …"
			}
			shown := []rune(strings.ReplaceAll(sig, "`", "'"))
			fmt.Fprintf(w, "- `%s` — %d: %s%s\n", string(shown[:min(200, len(shown))]), len(ds), strings.Join(names, ", "), more)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Close()
}
