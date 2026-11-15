// cpancompat measures how much of CPAN runs unmodified on go-perl.
//
// The work is split into steps that each read and write plain files in a
// data directory (outside the repository), so every step can be re-run or
// resumed on its own:
//
//	cpancompat fetch   download the CPAN package index and the MetaCPAN
//	                   metadata of every latest release
//	cpancompat testers download the CPAN Testers reports made on one perl
//	                   version (-perl), the version the interpreter is
//	                   built from
//	cpancompat graph   join them into one record per distribution and
//	                   derive the dependency graph (-perl: with the reports
//	                   each release got on that perl version)
//	cpancompat native  find the distributions with native (XS) code among
//	                   the most depended-upon ones (-top N)
//	cpancompat image   build the container image distributions are tested
//	                   in, around a gperl binary built for linux, or the
//	                   same environment around an official perl image (the
//	                   reference the interpreter is compared with)
//	cpancompat run     configure, build, test and install distributions on
//	                   go-perl, in dependency order, one container each;
//	                   results accumulate in runs/<name>/results.jsonl (and
//	                   the settings of every invocation in
//	                   invocations.jsonl), and an interrupted run continues
//	                   where it stopped. The
//	                   containers run CPAN code, which may be malicious: as
//	                   an unprivileged user without capabilities, with no
//	                   writable directory of this machine, kept from the
//	                   LAN and the hosts around them, and without network
//	                   while they test
//	cpancompat report  summarize a run: outcomes, coverage against the
//	                   distributions that pass on perl, why each
//	                   distribution that did not pass failed
//	                   (failures.jsonl, failures.md) and, given a run of
//	                   perl itself in the same harness (-reference-data,
//	                   -reference-run), how the two compare
//	cpancompat refset  write the CPAN snapshot and the reference set of a
//	                   data directory into the results directory, the
//	                   files the repository keeps (-results)
//	cpancompat export  write a run into the results directory: what was
//	                   measured and how (meta.json), its results, its
//	                   failure records and the end of each failing log
//
// The data directory is given with -data or the CPANCOMPAT_DATA
// environment variable.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  cpancompat fetch  [-data dir]
  cpancompat testers [-data dir] -perl version
  cpancompat graph  [-data dir] [-perl version]
  cpancompat native [-data dir] [-top n]
  cpancompat image  [-data dir] (-gperl binary | -perl-image image) [-image tag]
  cpancompat run    [-data dir] -run name [-image tag] [-top n | -dists a,b,...]
                    [-jobs n] [-cpus n] [-memory size] [-wall duration]
                    [-test-limit duration] [-interpreter label] [-redo outcome,...]
  cpancompat report [-data dir] -run name
                    [-reference-data dir -reference-run name]
  cpancompat refset [-data dir] -results dir
  cpancompat export [-data dir] -run name -results dir -interpreter-name name
                    [-interpreter-version v] [-interpreter-commit c]
                    [-base-image image] [-compared-with run]
                    [-provisional -note text] [-invocations file]
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	data := fs.String("data", os.Getenv("CPANCOMPAT_DATA"), "data directory")
	top := fs.Int("top", 0, "native, run: only the n most depended-upon distributions (native: default 1000; run: default all)")
	gperl := fs.String("gperl", "", "image: the gperl binary, built for linux")
	perl := fs.String("perl", "", "testers, graph: the perl version the interpreter is built from, as CPAN Testers names it (e.g. 5.44.0)")
	perlImage := fs.String("perl-image", "", "image: build the reference image around this official perl image instead of gperl")
	image := fs.String("image", "cpancompat:latest", "image, run: the container image tag")
	run := fs.String("run", "", "run, report: the name of the run")
	only := fs.String("dists", "", "run: only these distributions (comma separated), with what they need")
	jobs := fs.Int("jobs", 2, "run: how many distributions to run at once")
	cpus := fs.String("cpus", "2", "run: CPUs of each container")
	memory := fs.String("memory", "2g", "run: memory of each container")
	wall := fs.Duration("wall", 2*time.Hour, "run: how long one container may run")
	testLimit := fs.Duration("test-limit", 20*time.Minute, "run: how long the test phase of one distribution may run (whole seconds)")
	interpreter := fs.String("interpreter", "", "run: a label for the interpreter build, kept in every result")
	redo := fs.String("redo", "", "run: outcomes to run again (comma separated) instead of keeping their recorded result")
	referenceData := fs.String("reference-data", "", "report: the data directory of a run of perl itself to compare with")
	referenceRunName := fs.String("reference-run", "", "report: the name of that run")
	results := fs.String("results", "", "refset, export: the results directory the repository keeps")
	interpreterName := fs.String("interpreter-name", "", "export: the interpreter the run measured (go-perl or perl)")
	interpreterVersion := fs.String("interpreter-version", "", "export: its version")
	interpreterCommit := fs.String("interpreter-commit", "", "export: the revision it was built from")
	baseImage := fs.String("base-image", "", "export: the image the survey image was built on")
	comparedWith := fs.String("compared-with", "", "export: the run of perl itself this run is compared with")
	provisional := fs.Bool("provisional", false, "export: the run is provisional")
	note := fs.String("note", "", "export: what the run is, in a sentence or two")
	invocations := fs.String("invocations", "", "export: a JSON array of the invocations of a run made before run recorded them")
	_ = fs.Parse(os.Args[2:])
	if *data == "" {
		fmt.Fprintln(os.Stderr, "cpancompat: no data directory (-data or CPANCOMPAT_DATA)")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "fetch":
		err = fetch(*data)
	case "testers":
		err = fetchTesters(*data, *perl)
	case "graph":
		err = buildGraph(*data, *perl)
	case "native":
		if *top == 0 {
			*top = 1000
		}
		err = scanNative(*data, *top)
	case "image":
		err = buildImage(*data, *gperl, *perlImage, *image)
	case "run":
		if *run == "" {
			usage()
		}
		opts := runOptions{
			dataDir: *data, run: *run, image: *image, interpreter: *interpreter,
			top: *top, jobs: *jobs, cpus: *cpus, memory: *memory, wall: *wall,
			testLimit: *testLimit, redo: map[string]bool{},
		}
		if *only != "" {
			opts.only = strings.Split(*only, ",")
		}
		if *redo != "" {
			for _, outcome := range strings.Split(*redo, ",") {
				opts.redo[outcome] = true
			}
		}
		err = runSurvey(opts)
	case "report":
		if *run == "" {
			usage()
		}
		if (*referenceData == "") != (*referenceRunName == "") {
			usage()
		}
		err = report(*data, *run, referenceRun{dataDir: *referenceData, run: *referenceRunName}, os.Stdout)
	case "refset":
		if *results == "" {
			usage()
		}
		err = exportReferenceSet(*data, *results)
	case "export":
		if *run == "" || *results == "" {
			usage()
		}
		err = exportRun(exportOptions{
			dataDir: *data, run: *run, results: *results,
			interpreter: interpreterMeta{Name: *interpreterName, Version: *interpreterVersion, Commit: *interpreterCommit},
			baseImage:   *baseImage, provisional: *provisional, note: *note, comparedWith: *comparedWith,
			invocations: *invocations,
		})
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cpancompat: %v\n", err)
		os.Exit(1)
	}
}
