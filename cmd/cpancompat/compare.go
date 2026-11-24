package main

// The reference set, how a run went on it, and how two runs on the same
// reference set compare: a run of the interpreter under survey (the
// subject) and a run of perl itself in the same harness (the reference
// run).

import (
	"regexp"
	"sort"
	"unicode/utf8"
)

// refDist is one distribution of the reference set: the latest releases
// whose CPAN Testers reports on the perl version the interpreter is built
// from pass more often than they fail on Linux (dist.passesOnPerl).
type refDist struct {
	Dist      string `json:"dist"`
	Release   string `json:"release"`
	LinuxPass int    `json:"linux_pass"`
	LinuxFail int    `json:"linux_fail"`
	// DependentsTotal counts the distributions of the snapshot that need
	// this one, directly or indirectly.
	DependentsTotal int `json:"dependents_total"`
}

// referenceSet returns the reference set of a graph, sorted by name.
func referenceSet(dists []dist) []refDist {
	var set []refDist
	for i := range dists {
		d := &dists[i]
		if !d.passesOnPerl() {
			continue
		}
		set = append(set, refDist{
			Dist: d.Dist, Release: d.Release,
			LinuxPass: d.OnPerl.LinuxPass, LinuxFail: d.OnPerl.LinuxFail,
			DependentsTotal: d.DependentsTotal,
		})
	}
	sort.Slice(set, func(i, j int) bool { return set[i].Dist < set[j].Dist })
	return set
}

// runData is what the comparison needs of one run: the last result of each
// distribution and the failure records of the ones that did not pass.
type runData struct {
	name     string
	latest   map[string]*result
	failures map[string]*failure
}

func newRunData(name string, latest []*result, failures []failure) *runData {
	r := &runData{name: name, latest: map[string]*result{}, failures: map[string]*failure{}}
	for _, res := range latest {
		r.latest[res.Dist] = res
	}
	for i := range failures {
		r.failures[failures[i].Dist] = &failures[i]
	}
	return r
}

// outcome is the outcome of the distribution in the run, "" when the run
// has no record of it.
func (r *runData) outcome(name string) string {
	if res := r.latest[name]; res != nil {
		return res.Outcome
	}
	return ""
}

// coverageOn counts how the run went on the reference set.
func coverageOn(set []refDist, r *runData) coverage {
	c := coverage{Distributions: len(set), Outcomes: map[string]int{}}
	for _, d := range set {
		if o := r.outcome(d.Dist); o != "" {
			c.Recorded++
			c.Outcomes[o]++
		}
	}
	if c.Recorded > 0 {
		c.PassShareOfRecorded = float64(c.Outcomes[outcomePass]) / float64(c.Recorded)
	}
	if c.Distributions > 0 {
		c.PassShareOfAll = float64(c.Outcomes[outcomePass]) / float64(c.Distributions)
	}
	return c
}

// bucket is how one reference distribution went in the two runs. A
// distribution a run has no record of counts as not passing there.
type bucket string

const (
	bucketBothPass           bucket = "both_pass"
	bucketSubjectOnlyFails   bucket = "subject_only_fails"
	bucketBothFail           bucket = "both_fail"
	bucketReferenceOnlyFails bucket = "reference_only_fails"
)

// buckets lists the buckets in the order they are presented.
var buckets = []bucket{bucketBothPass, bucketSubjectOnlyFails, bucketBothFail, bucketReferenceOnlyFails}

func bucketOf(subject, reference string) bucket {
	switch {
	case subject == outcomePass && reference == outcomePass:
		return bucketBothPass
	case reference == outcomePass:
		return bucketSubjectOnlyFails
	case subject == outcomePass:
		return bucketReferenceOnlyFails
	}
	return bucketBothFail
}

// comparison is how the subject run went against the reference run on the
// reference set.
type comparison struct {
	Subject       string         `json:"subject"`
	Reference     string         `json:"reference"`
	Distributions int            `json:"distributions"`
	Buckets       map[bucket]int `json:"buckets"`
	// SubjectPasses and ReferencePasses count the passing distributions
	// of each run; ShareOfReferencePasses is the share of the reference
	// run's passing distributions that pass in the subject run too.
	SubjectPasses          int     `json:"subject_passes"`
	ReferencePasses        int     `json:"reference_passes"`
	ShareOfReferencePasses float64 `json:"share_of_reference_passes"`
	// RecordedByBoth counts the distributions both runs have a record of
	// (all of them once both runs are complete), and BucketsRecordedByBoth
	// their buckets.
	RecordedByBoth        int            `json:"recorded_by_both"`
	BucketsRecordedByBoth map[bucket]int `json:"buckets_recorded_by_both"`
	// SubjectOnlyOutcomes counts the subject run's outcomes of the
	// distributions only the subject run fails ("" for no record).
	SubjectOnlyOutcomes map[string]int `json:"subject_only_outcomes"`
	// RootBlockers are the distributions at the ends of the chains of
	// blocking in the subject run, by how many distributions only the
	// subject run fails they block.
	RootBlockers []rootBlocker `json:"root_blockers"`
	// BlockedGroups groups the blocked distributions only the subject run
	// fails by each distribution they name as blocking them; OwnGroups the
	// others by outcome, cause and signature.
	BlockedGroups []distGroup `json:"blocked_groups"`
	OwnGroups     []distGroup `json:"own_groups"`
}

// rootBlocker is a distribution that did not install in the subject run
// and blocks others there.
type rootBlocker struct {
	Dist string `json:"dist"`
	// Blocked counts the distributions only the subject run fails whose
	// chains of blocking end at this one.
	Blocked          int    `json:"blocked"`
	SubjectOutcome   string `json:"subject_outcome"`
	ReferenceOutcome string `json:"reference_outcome"`
	Cause            cause  `json:"cause,omitempty"`
	DependentsTotal  int    `json:"dependents_total"`
}

// distGroup is a group of distributions that failed alike, the ones most
// depended upon first.
type distGroup struct {
	Blocker   string   `json:"blocker,omitempty"`
	Outcome   string   `json:"outcome,omitempty"`
	Cause     cause    `json:"cause,omitempty"`
	Signature string   `json:"signature,omitempty"`
	Dists     []string `json:"dists"`
}

var (
	signatureAtLine = regexp.MustCompile(` at \S+ line N\.?`)
	signaturePath   = regexp.MustCompile(`/[\w./-]+`)
)

// groupSignature is a signature with what differs between test files
// replaced (the file and line of a message, paths), cut to 140 characters.
func groupSignature(sig string) string {
	sig = signatureAtLine.ReplaceAllString(sig, " at FILE")
	sig = signaturePath.ReplaceAllString(sig, "/PATH")
	if utf8.RuneCountInString(sig) > 140 {
		sig = string([]rune(sig)[:140])
	}
	return sig
}

// compareRuns compares the subject run with the reference run on set.
func compareRuns(set []refDist, subject, reference *runData) comparison {
	c := comparison{
		Subject: subject.name, Reference: reference.name, Distributions: len(set),
		Buckets: map[bucket]int{}, BucketsRecordedByBoth: map[bucket]int{},
		SubjectOnlyOutcomes: map[string]int{},
	}
	dependents := map[string]int{}
	for _, d := range set {
		dependents[d.Dist] = d.DependentsTotal
	}
	type key struct {
		blocker, outcome, signature string
		cause                       cause
	}
	groups := map[key][]string{}
	roots := map[string]int{}
	for _, d := range set {
		so, ro := subject.outcome(d.Dist), reference.outcome(d.Dist)
		b := bucketOf(so, ro)
		c.Buckets[b]++
		if so != "" && ro != "" {
			c.RecordedByBoth++
			c.BucketsRecordedByBoth[b]++
		}
		if b != bucketSubjectOnlyFails {
			continue
		}
		c.SubjectOnlyOutcomes[so]++
		f := subject.failures[d.Dist]
		if f == nil {
			continue
		}
		for _, r := range f.RootBlockers {
			roots[r]++
		}
		if so == outcomeBlocked && len(f.BlockedBy) > 0 {
			for _, b := range f.BlockedBy {
				groups[key{blocker: b}] = append(groups[key{blocker: b}], d.Dist)
			}
			continue
		}
		k := key{outcome: so, cause: f.Cause, signature: groupSignature(f.Signature)}
		groups[k] = append(groups[k], d.Dist)
	}
	c.SubjectPasses = c.Buckets[bucketBothPass] + c.Buckets[bucketReferenceOnlyFails]
	c.ReferencePasses = c.Buckets[bucketBothPass] + c.Buckets[bucketSubjectOnlyFails]
	if c.ReferencePasses > 0 {
		c.ShareOfReferencePasses = float64(c.Buckets[bucketBothPass]) / float64(c.ReferencePasses)
	}
	for name, n := range roots {
		rb := rootBlocker{
			Dist: name, Blocked: n,
			SubjectOutcome: subject.outcome(name), ReferenceOutcome: reference.outcome(name),
		}
		if f := subject.failures[name]; f != nil {
			rb.Cause, rb.DependentsTotal = f.Cause, f.DependentsTotal
		}
		if d, ok := dependents[name]; ok {
			rb.DependentsTotal = d
		}
		c.RootBlockers = append(c.RootBlockers, rb)
	}
	sort.Slice(c.RootBlockers, func(i, j int) bool {
		a, b := c.RootBlockers[i], c.RootBlockers[j]
		if a.Blocked != b.Blocked {
			return a.Blocked > b.Blocked
		}
		return a.Dist < b.Dist
	})
	for k, members := range groups {
		sort.Slice(members, func(i, j int) bool {
			if dependents[members[i]] != dependents[members[j]] {
				return dependents[members[i]] > dependents[members[j]]
			}
			return members[i] < members[j]
		})
		g := distGroup{Blocker: k.blocker, Outcome: k.outcome, Cause: k.cause, Signature: k.signature, Dists: members}
		if k.blocker != "" {
			c.BlockedGroups = append(c.BlockedGroups, g)
		} else {
			c.OwnGroups = append(c.OwnGroups, g)
		}
	}
	for _, gs := range [][]distGroup{c.BlockedGroups, c.OwnGroups} {
		sort.Slice(gs, func(i, j int) bool {
			a, b := gs[i], gs[j]
			if len(a.Dists) != len(b.Dists) {
				return len(a.Dists) > len(b.Dists)
			}
			if a.Blocker != b.Blocker {
				return a.Blocker < b.Blocker
			}
			if a.Outcome != b.Outcome {
				return a.Outcome < b.Outcome
			}
			if a.Cause != b.Cause {
				return a.Cause < b.Cause
			}
			return a.Signature < b.Signature
		})
	}
	return c
}

// causeCount is how many of a run's own failures (not blocked) on the
// reference set have one cause, and how many of those the reference run
// passes.
type causeCount struct {
	Cause           cause `json:"cause"`
	Distributions   int   `json:"distributions"`
	ReferencePasses int   `json:"reference_passes"`
}

// causeCounts counts the causes of the run's own failures on set, the most
// common first; reference may be nil.
func causeCounts(set []refDist, r, reference *runData) []causeCount {
	counts := map[cause]*causeCount{}
	for _, d := range set {
		f := r.failures[d.Dist]
		if f == nil || f.Cause == causeBlocked {
			continue
		}
		c := counts[f.Cause]
		if c == nil {
			c = &causeCount{Cause: f.Cause}
			counts[f.Cause] = c
		}
		c.Distributions++
		if reference != nil && reference.outcome(d.Dist) == outcomePass {
			c.ReferencePasses++
		}
	}
	out := make([]causeCount, 0, len(counts))
	for _, c := range counts {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Distributions != out[j].Distributions {
			return out[i].Distributions > out[j].Distributions
		}
		return out[i].Cause < out[j].Cause
	})
	return out
}
