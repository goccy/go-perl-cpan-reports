package main

// Known causes: the causes of failures whose logs do not tell them, given by
// analysis and recorded as data in known-causes.json, which is built into
// the command. Each entry names a distribution, the cause, the test files
// the analysis attributed to it and the evidence for that.
//
// The entries are about go-perl: they are applied to go-perl runs only, and
// only to a distribution whose tests failed (outcome fail) in no test file
// but the entry's. Any other record keeps the cause its own evidence tells.

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
)

// The names of the interpreters a run measures (-interpreter-name).
const (
	interpreterGoPerl = "go-perl"
	interpreterPerl   = "perl"
)

//go:embed known-causes.json
var knownCausesFile []byte

// knownCause is one entry of known-causes.json.
type knownCause struct {
	Dist  string `json:"dist"`
	Cause cause  `json:"cause"`
	// Tests are the test files the analysis attributed to the cause.
	Tests []string `json:"tests"`
	// Evidence says what ties them to it.
	Evidence string `json:"evidence"`
}

// knownCauseKinds are the causes an entry may name: the known limitation of
// fork, whose message the evidence shows only where the child's output
// reached the log; a child whose output was discarded leaves none.
var knownCauseKinds = []cause{causeForkChildNativeCode}

// parseKnownCauses reads the entries of a known-causes file, by
// distribution. Every entry must name a distribution no other entry names,
// one of knownCauseKinds, at least one test file and its evidence.
func parseKnownCauses(raw []byte) (map[string]*knownCause, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var entries []knownCause
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("known causes: %w", err)
	}
	kinds := map[cause]bool{}
	for _, c := range knownCauseKinds {
		kinds[c] = true
	}
	known := make(map[string]*knownCause, len(entries))
	for i := range entries {
		k := &entries[i]
		switch {
		case k.Dist == "":
			return nil, fmt.Errorf("known causes: entry %d names no distribution", i)
		case known[k.Dist] != nil:
			return nil, fmt.Errorf("known causes: %s has more than one entry", k.Dist)
		case !kinds[k.Cause]:
			return nil, fmt.Errorf("known causes: %s: %q is not a cause analysis gives", k.Dist, k.Cause)
		case len(k.Tests) == 0:
			return nil, fmt.Errorf("known causes: %s: no test files", k.Dist)
		case k.Evidence == "":
			return nil, fmt.Errorf("known causes: %s: no evidence", k.Dist)
		}
		files := map[string]bool{}
		for _, f := range k.Tests {
			if f == "" || files[f] {
				return nil, fmt.Errorf("known causes: %s: test file %q is empty or listed twice", k.Dist, f)
			}
			files[f] = true
		}
		known[k.Dist] = k
	}
	return known, nil
}

// knownCauses returns the entries of the known-causes.json the command was
// built with.
func knownCauses() (map[string]*knownCause, error) {
	return parseKnownCauses(knownCausesFile)
}

// applyKnownCauses gives the failure records of the run, a run of
// interpreter, the cause of their distribution's entry in known, where the
// entry covers the failure: see knownCause.covers. A run of any interpreter
// but go-perl keeps the causes its evidence tells.
func (r *runData) applyKnownCauses(known map[string]*knownCause, interpreter string) {
	if interpreter != interpreterGoPerl {
		return
	}
	for name, f := range r.failures {
		if k := known[name]; k != nil && k.covers(r.latest[name]) {
			f.Cause = k.Cause
		}
	}
}

// covers reports whether the entry accounts for the failure of res: the
// distribution built and its tests failed (outcome fail), and every test
// file that failed, of which there is at least one, is among the entry's.
// A distribution that did not pass for another reason, or that failed in a
// test file the analysis did not attribute, is not covered.
func (k *knownCause) covers(res *result) bool {
	if res == nil || res.Outcome != outcomeFail {
		return false
	}
	listed := make(map[string]bool, len(k.Tests))
	for _, f := range k.Tests {
		listed[f] = true
	}
	failed := false
	for _, t := range res.Tests {
		if !t.failed() {
			continue
		}
		if !listed[t.File] {
			return false
		}
		failed = true
	}
	return failed
}
