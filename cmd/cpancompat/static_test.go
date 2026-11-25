package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// testELF describes a small ELF64 file for the tests: its program
// interpreter, the shared libraries it names, and the dynamic symbols it
// defines, needs, and refers to weakly.
type testELF struct {
	interp                   string
	needed                   []string
	defined, undefined, weak []string
}

// bytes lays the file out: the header, a PT_INTERP program header when it
// has an interpreter, the sections' contents, and the section headers.
func (e testELF) bytes() []byte {
	le := binary.LittleEndian
	add := func(tab *[]byte, s string) uint32 {
		off := len(*tab)
		*tab = append(append(*tab, s...), 0)
		return uint32(off)
	}
	type section struct {
		name             string
		typ              elf.SectionType
		data             []byte
		link             uint32
		entsize          uint64
		nameOff, dataOff uint64
	}
	sections := []*section{{}}
	index := func(name string) uint32 {
		for i, s := range sections {
			if s.name == name {
				return uint32(i)
			}
		}
		return 0
	}
	if e.interp != "" {
		sections = append(sections, &section{name: ".interp", typ: elf.SHT_PROGBITS, data: append([]byte(e.interp), 0)})
	}
	dynamic := len(e.needed)+len(e.defined)+len(e.undefined)+len(e.weak) > 0
	if dynamic {
		dynstr := []byte{0}
		var syms bytes.Buffer
		syms.Write(make([]byte, 24))
		sym := func(name string, bind elf.SymBind, shndx uint16) {
			var b [24]byte
			le.PutUint32(b[0:], add(&dynstr, name))
			b[4] = byte(bind)<<4 | byte(elf.STT_FUNC)
			le.PutUint16(b[6:], shndx)
			syms.Write(b[:])
		}
		// .text follows .dynstr, .dynsym and .dynamic.
		text := uint16(len(sections) + 3)
		for _, s := range e.defined {
			sym(s, elf.STB_GLOBAL, text)
		}
		for _, s := range e.undefined {
			sym(s, elf.STB_GLOBAL, uint16(elf.SHN_UNDEF))
		}
		for _, s := range e.weak {
			sym(s, elf.STB_WEAK, uint16(elf.SHN_UNDEF))
		}
		var dyn bytes.Buffer
		for _, n := range e.needed {
			var b [16]byte
			le.PutUint64(b[0:], uint64(elf.DT_NEEDED))
			le.PutUint64(b[8:], uint64(add(&dynstr, n)))
			dyn.Write(b[:])
		}
		dyn.Write(make([]byte, 16))
		str := uint32(len(sections))
		sections = append(sections,
			&section{name: ".dynstr", typ: elf.SHT_STRTAB, data: dynstr},
			&section{name: ".dynsym", typ: elf.SHT_DYNSYM, data: syms.Bytes(), link: str, entsize: 24},
			&section{name: ".dynamic", typ: elf.SHT_DYNAMIC, data: dyn.Bytes(), link: str, entsize: 16},
			&section{name: ".text", typ: elf.SHT_PROGBITS, data: []byte{0, 0, 0, 0}})
	}
	shstr := []byte{0}
	for _, s := range sections[1:] {
		s.nameOff = uint64(add(&shstr, s.name))
	}
	shstrtab := &section{name: ".shstrtab", typ: elf.SHT_STRTAB}
	shstrtab.nameOff = uint64(add(&shstr, ".shstrtab"))
	shstrtab.data = shstr
	sections = append(sections, shstrtab)

	phnum := 0
	if e.interp != "" {
		phnum = 1
	}
	off := uint64(64 + 56*phnum)
	for _, s := range sections[1:] {
		off = (off + 7) &^ 7
		s.dataOff = off
		off += uint64(len(s.data))
	}
	shoff := (off + 7) &^ 7
	out := make([]byte, shoff+uint64(64*len(sections)))
	copy(out, []byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)})
	le.PutUint16(out[16:], uint16(elf.ET_DYN))
	le.PutUint16(out[18:], uint16(elf.EM_AARCH64))
	le.PutUint32(out[20:], uint32(elf.EV_CURRENT))
	if phnum > 0 {
		le.PutUint64(out[32:], 64)
	}
	le.PutUint64(out[40:], shoff)
	le.PutUint16(out[52:], 64)
	le.PutUint16(out[54:], 56)
	le.PutUint16(out[56:], uint16(phnum))
	le.PutUint16(out[58:], 64)
	le.PutUint16(out[60:], uint16(len(sections)))
	le.PutUint16(out[62:], uint16(len(sections)-1))
	if phnum > 0 {
		s := sections[index(".interp")]
		ph := out[64:]
		le.PutUint32(ph[0:], uint32(elf.PT_INTERP))
		le.PutUint64(ph[8:], s.dataOff)
		le.PutUint64(ph[32:], uint64(len(s.data)))
		le.PutUint64(ph[40:], uint64(len(s.data)))
	}
	for i, s := range sections[1:] {
		copy(out[s.dataOff:], s.data)
		sh := out[shoff+uint64(64*(i+1)):]
		le.PutUint32(sh[0:], uint32(s.nameOff))
		le.PutUint32(sh[4:], uint32(s.typ))
		le.PutUint64(sh[24:], s.dataOff)
		le.PutUint64(sh[32:], uint64(len(s.data)))
		le.PutUint32(sh[40:], s.link)
		le.PutUint64(sh[48:], 1)
		le.PutUint64(sh[56:], s.entsize)
	}
	return out
}

func writeTestELF(t *testing.T, name string, e testELF) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, e.bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBinaryOf(t *testing.T) {
	dir := t.TempDir()
	writeTestELF(t, filepath.Join(dir, "static"), testELF{})
	writeTestELF(t, filepath.Join(dir, "dynamic"), testELF{interp: "/lib/ld-linux-aarch64.so.1", needed: []string{"libc.so.6", "libm.so.6"}})
	got, err := binaryOf(filepath.Join(dir, "static"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Interp != "" || len(got.Needed) != 0 || got.Needed == nil || got.Bytes == 0 {
		t.Errorf("static binary: %+v", got)
	}
	got, err = binaryOf(filepath.Join(dir, "dynamic"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Interp != "/lib/ld-linux-aarch64.so.1" || !reflect.DeepEqual(got.Needed, []string{"libc.so.6", "libm.so.6"}) {
		t.Errorf("dynamic binary: %+v", got)
	}
	if _, err := binaryOf(filepath.Join(dir, "missing")); err == nil {
		t.Errorf("a missing binary has no error")
	}
}

func TestTreeModules(t *testing.T) {
	tree := t.TempDir()
	auto := filepath.Join(tree, "abi8", "linux_arm64", "auto")
	writeTestELF(t, filepath.Join(auto, "Net", "SSLeay", "SSLeay.so"), testELF{
		needed: []string{"libssl.so.3", "libcrypto.so.3", "libc.so.6"}, defined: []string{"boot_Net__SSLeay"},
		undefined: []string{"SSL_new", "strlen"}, weak: []string{"__gmon_start__"},
	})
	writeFiles(t, tree, map[string]string{
		"abi8/linux_arm64/auto/Net/SSLeay/SSLeay.ldflags": "-L/usr/lib/aarch64-linux-gnu\n-lssl\n-lcrypto",
		"archives.txt": "/usr/lib/aarch64-linux-gnu/libssl.a\n",
	})
	mods, err := treeModules(tree, "Net-SSLeay")
	if err != nil {
		t.Fatal(err)
	}
	if len(mods) != 1 {
		t.Fatalf("got %d modules, want 1", len(mods))
	}
	m := mods[0]
	if m.pkg != "Net::SSLeay" || m.dist != "Net-SSLeay" {
		t.Errorf("module %s of %s", m.pkg, m.dist)
	}
	if !reflect.DeepEqual(m.ldflags, []string{"-L/usr/lib/aarch64-linux-gnu", "-lssl", "-lcrypto"}) {
		t.Errorf("ldflags %q", m.ldflags)
	}
	if !reflect.DeepEqual(m.needed, []string{"libssl.so.3", "libcrypto.so.3", "libc.so.6"}) {
		t.Errorf("needed %q", m.needed)
	}
	// A weak reference needs nothing.
	if !reflect.DeepEqual(m.undefined, []string{"SSL_new", "strlen"}) || !m.defined["boot_Net__SSLeay"] {
		t.Errorf("undefined %q, defined %v", m.undefined, m.defined)
	}
	if !m.archives["/usr/lib/aarch64-linux-gnu/libssl.a"] {
		t.Errorf("archives %v", m.archives)
	}
	if _, err := treeModules(filepath.Join(tree, "nowhere"), "X"); err == nil {
		t.Errorf("a tree that was not collected has no error")
	}
}

func TestExternalLibraries(t *testing.T) {
	system := map[string]string{"libssl.a": "/usr/lib/aarch64-linux-gnu/libssl.a", "libz.a": "/usr/lib/aarch64-linux-gnu/libz.a"}
	mods := []moduleELF{
		{pkg: "Net::SSLeay", dist: "Net-SSLeay", ldflags: []string{"-lssl", "-lcrypto", "-lm"}, needed: []string{"libssl.so.3", "libc.so.6"}},
		{pkg: "Alien::Thing::XS", dist: "Thing", ldflags: []string{"-L/opt/gperl/mod/Alien-Thing@1/gperl/lib/auto/share/dist/Alien-Thing/lib", "-lthing", "-l:libz.a", "-lstdc++"},
			archives: map[string]bool{"/opt/gperl/mod/Alien-Thing@1/gperl/lib/auto/share/dist/Alien-Thing/lib/libthing.a": true}},
	}
	got := externalLibraries(mods, system)
	want := []externalLib{
		{Name: "crypto", Dist: "Net-SSLeay"},
		{Name: "ssl", Dist: "Net-SSLeay", From: "system", StaticArchive: true},
		{Name: "thing", Dist: "Thing", From: "module_directory", StaticArchive: true},
		{Name: "z", Dist: "Thing", From: "system", StaticArchive: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("externalLibraries:\n got %+v\nwant %+v", got, want)
	}
}

func TestLinkFailure(t *testing.T) {
	rt := runtimeSymbols{"c": {"strlen": true}, "m": {"sqrt": true}, "crypt": {"crypt": true}}
	base := func() []moduleELF {
		return []moduleELF{
			{pkg: "A", dist: "Own", defined: map[string]bool{"a_helper": true}},
			{pkg: "B", dist: "Dep", defined: map[string]bool{"b_export": true}},
		}
	}
	mods := base()
	mods[1].ldflags = []string{"/usr/lib/libfoo.so"}
	if r, by := linkFailure("Own", mods, rt, nil); r != reasonSharedLibraryPathDropped || !reflect.DeepEqual(by, []string{"Dep"}) {
		t.Errorf("a shared library by its path: %s %v", r, by)
	}
	if r, by := linkFailure("Own", base(), rt, []externalLib{{Name: "foo", Dist: "Own"}}); r != reasonNeedsSharedLibrary || len(by) != 0 {
		t.Errorf("no static archive: %s %v", r, by)
	}
	mods = base()
	mods[0].undefined = []string{"b_export", "strlen"}
	if r, by := linkFailure("Own", mods, rt, nil); r != reasonCrossModuleSymbols || !reflect.DeepEqual(by, []string{"Dep"}) {
		t.Errorf("a symbol of another module: %s %v", r, by)
	}
	mods = base()
	mods[1].undefined = []string{"sqrt"}
	if r, by := linkFailure("Own", mods, rt, nil); r != reasonCRuntimeNotLinked || !reflect.DeepEqual(by, []string{"Dep"}) {
		t.Errorf("libm not linked: %s %v", r, by)
	}
	mods[1].ldflags = []string{"-lm"}
	if r, _ := linkFailure("Own", mods, rt, nil); r != reasonLinkFailed {
		t.Errorf("libm linked: %s", r)
	}
}

func TestGperlTreeName(t *testing.T) {
	for path, want := range map[string]string{
		"R/RU/RURBAN/Cpanel-JSON-XS-4.38.tar.gz": "Cpanel-JSON-XS@4.38",
		"E/ET/ETHER/Moose-2.2207.tar.gz":         "Moose@2.2207",
		"X/XY/XYZ/Foo-v1.2.3.tgz":                "Foo@v1.2.3",
		"X/XY/XYZ/Foo-Bar-0.01_02.zip":           "Foo-Bar@0.01_02",
		"X/XY/XYZ/Foo-1.0.tar.bz2":               "Foo@1.0",
	} {
		if got, err := gperlTreeName(path); err != nil || got != want {
			t.Errorf("gperlTreeName(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	for _, path := range []string{"A/AB/ABC/Acme-Version-Same-0.01-patched.tar.gz", "A/AB/ABC/Foo.tar.gz", "A/AB/ABC/Foo-1.0.tar.xz"} {
		if got, err := gperlTreeName(path); err == nil {
			t.Errorf("gperlTreeName(%q) = %q, want an error", path, got)
		}
	}
}

func TestMymetaRequirements(t *testing.T) {
	v2 := `{"meta-spec":{"version":2},"prereqs":{
		"configure":{"requires":{"ExtUtils::MakeMaker":"6.64"}},
		"runtime":{"requires":{"perl":"5.008","JSON::PP":0},"recommends":{"Cpanel::JSON::XS":"4"}},
		"test":{"requires":{"Test::More":"0.98"}},
		"develop":{"requires":{"Dist::Zilla":"6"}}}}`
	got, configures, err := mymetaRequirements([]byte(v2), staticPhases)
	if err != nil {
		t.Fatal(err)
	}
	want := []snapshotPackage{{"ExtUtils::MakeMaker", "6.64"}, {"JSON::PP", "0"}, {"perl", "5.008"}}
	if got = mergeRequirements(got); !reflect.DeepEqual(got, want) || !configures {
		t.Errorf("version 2: %v, configures %v; want %v, true", got, configures, want)
	}
	// A target's tests need the test phase's requirements too.
	got, _, err = mymetaRequirements([]byte(v2), testPhases)
	if want := []snapshotPackage{{"ExtUtils::MakeMaker", "6.64"}, {"JSON::PP", "0"}, {"Test::More", "0.98"}, {"perl", "5.008"}}; err != nil || !reflect.DeepEqual(mergeRequirements(got), want) {
		t.Errorf("version 2 with the test phase: %v, %v; want %v", mergeRequirements(got), err, want)
	}
	v1 := `{"meta-spec":{"version":"1.4"},"requires":{"Moo":"2"},"build_requires":{"Module::Build":"0.4"},"configure_requires":{"Module::Build":"0.38"}}`
	got, configures, err = mymetaRequirements([]byte(v1), staticPhases)
	if err != nil {
		t.Fatal(err)
	}
	want = []snapshotPackage{{"Module::Build", "0.38, 0.4"}, {"Moo", "2"}}
	if got = mergeRequirements(got); !reflect.DeepEqual(got, want) || !configures {
		t.Errorf("version 1.4: %v, configures %v; want %v, true", got, configures, want)
	}
	for _, meta := range []string{
		`{"meta-spec":{"version":2},"prereqs":{"runtime":{"requires":{"Moo":"2"}},"configure":{"recommends":{"Foo":"1"}}}}`,
		`{"meta-spec":{"version":"1.4"},"requires":{"Moo":"2"}}`,
	} {
		if _, configures, err := mymetaRequirements([]byte(meta), staticPhases); err != nil || configures {
			t.Errorf("%s: configures %v, %v; want false", meta, configures, err)
		}
	}
	if _, _, err := mymetaRequirements([]byte("{"), staticPhases); err == nil {
		t.Errorf("a MYMETA.json that does not parse has no error")
	}
}

func TestModulesFailure(t *testing.T) {
	treeDist := map[string]string{"Own@1": "Own", "Dep@1": "Dep", "Peer@1": "Peer"}
	for _, tc := range []struct {
		states        map[string]string
		timedOut      bool
		state, reason string
		blockedBy     []string
	}{
		{map[string]string{"Own@1": treeFailed, "Dep@1": treeBuilt}, false, staticNotStatic, reasonDistBuildFailed, nil},
		{map[string]string{"Own@1": treeRefused, "Dep@1": treeBuilt}, false, staticNotStatic, reasonLinkerOverridden, nil},
		// gperl failed before the build kept a log (its log tells why).
		{map[string]string{"Own@1": treeUnbuilt, "Dep@1": treeBuilt}, false, staticNotStatic, reasonDistBuildFailed, nil},
		{map[string]string{"Own@1": treeAbsent, "Dep@1": treeBuilt}, false, staticNotStatic, reasonDistBuildFailed, nil},
		{map[string]string{"Own@1": treeUnbuilt, "Dep@1": treeBuilt}, true, staticNotStatic, reasonModulesTimeout, nil},
		// A forced container: a peer of the cycle failed first, or after.
		{map[string]string{"Own@1": treeUnbuilt, "Peer@1": treeFailed, "Dep@1": treeBuilt}, false, staticBlocked, "", []string{"Peer"}},
		{map[string]string{"Own@1": treeBuilt, "Peer@1": treeRefused, "Dep@1": treeBuilt}, false, staticBlocked, "", []string{"Peer"}},
		// A forced container that ran out of time after the own tree.
		{map[string]string{"Own@1": treeBuilt, "Peer@1": treeUnbuilt, "Dep@1": treeBuilt}, true, staticBlocked, "", []string{"Peer"}},
		{map[string]string{"Own@1": treeBuilt, "Dep@1": treeBuilt}, false, "", "", nil},
	} {
		state, reason, blockedBy := modulesFailure("Own@1", tc.states, treeDist, tc.timedOut)
		if state != tc.state || reason != tc.reason || !reflect.DeepEqual(blockedBy, tc.blockedBy) {
			t.Errorf("modulesFailure(%v) = %q %q %v; want %q %q %v", tc.states, state, reason, blockedBy, tc.state, tc.reason, tc.blockedBy)
		}
	}
}

func TestReadTreeStates(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"ok.tsv": "A@1\tbuilt\nB@2\tfailed\n", "bad.tsv": "A@1\tcooked\n"})
	got, err := readTreeStates(filepath.Join(dir, "ok.tsv"))
	if err != nil || !reflect.DeepEqual(got, map[string]string{"A@1": treeBuilt, "B@2": treeFailed}) {
		t.Errorf("readTreeStates = %v, %v", got, err)
	}
	if _, err := readTreeStates(filepath.Join(dir, "bad.tsv")); err == nil {
		t.Errorf("an unknown state has no error")
	}
}

func TestStaticTargets(t *testing.T) {
	passed := []*dist{{Dist: "A", DependentsTotal: 1}, {Dist: "B", DependentsTotal: 5}, {Dist: "C", DependentsTotal: 5}}
	if got, err := staticTargets(passed, nil, 2); err != nil || !reflect.DeepEqual(got, []string{"B", "C"}) {
		t.Errorf("top 2: %v, %v", got, err)
	}
	if got, err := staticTargets(passed, []string{"A"}, 0); err != nil || !reflect.DeepEqual(got, []string{"A"}) {
		t.Errorf("-dists A: %v, %v", got, err)
	}
	if _, err := staticTargets(passed, []string{"D"}, 0); err == nil {
		t.Errorf("a distribution that did not pass is a target")
	}
}

// distArchive returns a .tar.gz archive of empty files with these names (a
// name ending in a slash is a directory), after a pax global header as git
// archive writes one.
func distArchive(t *testing.T, names ...string) string {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "0123abcd"}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		h := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644}
		if strings.HasSuffix(name, "/") {
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestExtractedHasBuildPL(t *testing.T) {
	for _, tc := range []struct {
		names []string
		want  bool
	}{
		{[]string{"Foo-1/", "Foo-1/Build.PL", "Foo-1/lib/Foo.pm"}, true},
		{[]string{"./", "./Foo-1/Build.PL", "./Foo-1/lib/Foo.pm"}, true},
		{[]string{"Foo-1/Makefile.PL", "Foo-1/lib/Foo.pm"}, false},
		// Below a directory that is not the only entry at the top.
		{[]string{"Foo-1/Build.PL", "README"}, false},
		{[]string{"Foo-1/Build.PL", "Bar/"}, false},
		{[]string{"Build.PL", "lib/Foo.pm"}, true},
		{[]string{"Build.PL"}, true},
		{[]string{"Foo-1/t/Build.PL"}, false},
	} {
		if got := extractedHasBuildPL(tc.names); got != tc.want {
			t.Errorf("extractedHasBuildPL(%q) = %v, want %v", tc.names, got, tc.want)
		}
	}
	names, err := archiveNames("Foo-1.tar.gz", []byte(distArchive(t, "Foo-1/", "Foo-1/Build.PL")))
	if err != nil || !reflect.DeepEqual(names, []string{"Foo-1/", "Foo-1/Build.PL"}) {
		t.Errorf("archiveNames = %q, %v; want the members without the pax global header", names, err)
	}
}

func TestModuleBuildConfigureRequirement(t *testing.T) {
	dataDir := t.TempDir()
	mk := func(name string, files []string, requires ...requirement) *dist {
		d := &dist{Dist: name, Release: name + "-1", Path: "A/AU/AUTHOR/" + name + "-1.tar.gz", Modules: []string{name}, Requires: requires}
		writeFiles(t, dataDir, map[string]string{"archives/" + d.Path: distArchive(t, files...)})
		return d
	}
	eumm := requirement{Module: "ExtUtils::MakeMaker", Phase: "configure", Relationship: "requires"}
	s := &staticRunner{
		opts: staticOptions{dataDir: dataDir},
		dists: map[string]*dist{
			"MB":           mk("MB", []string{"MB-1/Build.PL"}),
			"Declared":     mk("Declared", []string{"Declared-1/Build.PL"}, eumm),
			"Recommends":   mk("Recommends", []string{"Recommends-1/Build.PL"}, requirement{Module: "Foo", Phase: "configure", Relationship: "recommends"}),
			"MM":           mk("MM", []string{"MM-1/Makefile.PL"}),
			"version":      mk("version", []string{"version-1/Build.PL"}),
			"Built":        mk("Built", []string{"Built-1/Build.PL"}),
			"BuiltNone":    mk("BuiltNone", []string{"BuiltNone-1/Build.PL"}, eumm),
			"Module-Build": mk("Module-Build", []string{"Module-Build-1/Build.PL"}),
			"PerlOnly":     mk("PerlOnly", []string{"PerlOnly-1/Build.PL"}, requirement{Module: "perl", Version: "5.008", Phase: "configure", Relationship: "requires"}),
			"Corrupt":      mk("Corrupt", []string{"Corrupt-1/Build.PL"}),
			// gperl takes no such archive, so it reads no tree of it.
			"BadName": {Dist: "BadName", Release: "BadName-1.0-patched", Path: "A/AU/AUTHOR/BadName-1.0-patched.tar.gz", Modules: []string{"BadName"}},
		},
		byModule:     map[string]string{"Module::Build": "Module-Build"},
		provided:     map[string]string{},
		requirements: map[string]*distRequirements{}, verified: map[string]bool{}, treesDir: t.TempDir(),
	}
	// MYMETA.json stands in for the static metadata once a build left one.
	writeFiles(t, s.treesDir, map[string]string{
		"Built@1/MYMETA.json":     `{"meta-spec":{"version":2},"prereqs":{"configure":{"requires":{"Module::Build::Tiny":"0"}}}}`,
		"BuiltNone@1/MYMETA.json": `{"meta-spec":{"version":2},"prereqs":{"runtime":{"requires":{"Moo":"0"}}}}`,
	})
	s.dists["Corrupt"].SHA256 = strings.Repeat("0", 64)
	if _, err := s.requirementsOf("Corrupt"); err == nil {
		t.Errorf("Corrupt: an archive that does not match its checksum has no error")
	}
	for name, want := range map[string]bool{"MB": true, "Declared": false, "Recommends": true, "MM": false, "version": false, "Built": false, "BuiltNone": true, "PerlOnly": false, "BadName": false} {
		r, err := s.requirementsOf(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := slices.Contains(r.lines, snapshotPackage{"Module::Build", "0.38"})
		if got != want || got != slices.Contains(r.needs, "Module-Build") {
			t.Errorf("%s: lines %v, needs %v; want Module::Build 0.38 %v", name, r.lines, r.needs, want)
		}
	}
	// Module::Build needs no distribution but itself for it.
	if r, err := s.requirementsOf("Module-Build"); err != nil || !slices.Contains(r.lines, snapshotPackage{"Module::Build", "0.38"}) || len(r.needs) != 0 {
		t.Errorf("Module-Build: %+v, %v", r, err)
	}
}

func TestAppArchiveAndSnapshot(t *testing.T) {
	dataDir := t.TempDir()
	writeFiles(t, dataDir, map[string]string{
		"archives/H/HA/HAARG/Moo-2.005005.tar.gz":       distArchive(t, "Moo-2.005005/Makefile.PL"),
		"archives/H/HA/HAARG/Role-Tiny-2.002004.tar.gz": distArchive(t, "Role-Tiny-2.002004/Makefile.PL"),
	})
	s := &staticRunner{
		opts: staticOptions{dataDir: dataDir}, verified: map[string]bool{},
		dists: map[string]*dist{
			"Moo": {Dist: "Moo", Release: "Moo-2.005005", Path: "H/HA/HAARG/Moo-2.005005.tar.gz", Modules: []string{"Moo", "Moo::Role"},
				Requires: []requirement{{Module: "Role::Tiny", Version: "2.002004", Phase: "runtime", Relationship: "requires", Dist: "Role-Tiny"},
					{Module: "Test::Fatal", Phase: "test", Relationship: "requires", Dist: "Test-Fatal"}}},
			"Role-Tiny": {Dist: "Role-Tiny", Release: "Role-Tiny-2.002004", Path: "H/HA/HAARG/Role-Tiny-2.002004.tar.gz", Modules: []string{"Role::Tiny"},
				Requires: []requirement{{Module: "Test::Fatal", Phase: "test", Relationship: "requires", Dist: "Test-Fatal"}}},
			"Test-Fatal": {Dist: "Test-Fatal", Release: "Test-Fatal-0.017", Path: "R/RJ/RJBS/Test-Fatal-0.017.tar.gz", Modules: []string{"Test::Fatal"}},
		},
		byModule: map[string]string{"Moo": "Moo", "Moo::Role": "Moo", "Role::Tiny": "Role-Tiny", "Test::Fatal": "Test-Fatal"},
		versions: map[string]string{"Moo": "2.005005", "Moo::Role": "undef", "Role::Tiny": "2.002004", "Test::Fatal": "0.017"},
		provided: map[string]string{}, requirements: map[string]*distRequirements{}, testRequirements: map[string]*distRequirements{},
		treesDir: t.TempDir(),
	}
	closure, err := s.closure(&staticNode{d: s.dists["Moo"]})
	if err != nil || !reflect.DeepEqual(closure, []string{"Moo", "Role-Tiny"}) {
		t.Fatalf("closure = %v, %v (test requirements are not part of it)", closure, err)
	}
	// The test binary's closure has the target's own test requirements,
	// not those of what it needs.
	if c, tests, err := s.closures(&staticNode{d: s.dists["Moo"], target: true}); err != nil || !reflect.DeepEqual(c, closure) ||
		!reflect.DeepEqual(tests, []string{"Moo", "Role-Tiny", "Test-Fatal"}) {
		t.Errorf("closures = %v, %v, %v", c, tests, err)
	}
	if _, tests, err := s.closures(&staticNode{d: s.dists["Role-Tiny"]}); err != nil || tests != nil {
		t.Errorf("a dependency's test closure = %v, %v", tests, err)
	}
	snap, err := s.snapshotOf(closure)
	if err != nil {
		t.Fatal(err)
	}
	want := "# carton snapshot format: version 1.0\nDISTRIBUTIONS\n" +
		"  Moo-2.005005\n    pathname: H/HA/HAARG/Moo-2.005005.tar.gz\n    provides:\n      Moo 2.005005\n      Moo::Role undef\n" +
		"    requirements:\n      Role::Tiny 2.002004\n" +
		"  Role-Tiny-2.002004\n    pathname: H/HA/HAARG/Role-Tiny-2.002004.tar.gz\n    provides:\n      Role::Tiny 2.002004\n    requirements:\n"
	if string(snap) != want {
		t.Errorf("snapshot:\n%s\nwant:\n%s", snap, want)
	}
	if got := mainModules(s.dists["Moo"]); !reflect.DeepEqual(got, []string{"Moo"}) {
		t.Errorf("mainModules(Moo) = %v", got)
	}
	if got := mainModules(&dist{Dist: "libwww-perl", Modules: []string{"LWP", "LWP::UserAgent"}}); !reflect.DeepEqual(got, []string{"LWP", "LWP::UserAgent"}) {
		t.Errorf("mainModules(libwww-perl) = %v", got)
	}
	// MYMETA.json, once the build left one (and settle forgot what was
	// known), says what the distribution needs.
	writeFiles(t, s.treesDir, map[string]string{"Role-Tiny@2.002004/MYMETA.json": `{"meta-spec":{"version":2},"prereqs":{"runtime":{"requires":{"Moo":"0"}}}}`})
	delete(s.requirements, "Role-Tiny")
	closure, err = s.closure(&staticNode{d: s.dists["Role-Tiny"]})
	if err != nil || !reflect.DeepEqual(closure, []string{"Moo", "Role-Tiny"}) {
		t.Errorf("closure from MYMETA.json = %v, %v", closure, err)
	}
}

// fakeStatic is a scheduling test: what each distribution's container
// does, and what the scheduler made of it.
type fakeStatic struct {
	t      *testing.T
	s      *staticRunner
	order  []string
	result map[string]string   // dist -> state:reason its container ends with
	mymeta map[string]string   // dist -> MYMETA.json its build leaves
	starts map[string][]string // dist -> closures it started with
	forced map[string]bool     // dists started while their closure was not settled
	seen   map[string]bool
	// blockedBy is what a blocked container names.
	blockedBy map[string][]string
	// jobs are the last job of each distribution.
	jobs map[string]*staticJob
}

func (f *fakeStatic) attempt(job *staticJob) (*staticRecord, []string) {
	n := job.n
	f.order = append(f.order, n.d.Dist)
	if f.jobs != nil {
		f.jobs[n.d.Dist] = job
	}
	f.starts[n.d.Dist] = append(f.starts[n.d.Dist], strings.Join(job.closure, ","))
	// With one job at a time the coordinator waits for this one: the nodes
	// can be read here.
	for _, name := range job.closure {
		if m := f.s.nodes[name]; name != n.d.Dist && (m == nil || m.rec == nil) {
			if !n.forced {
				f.t.Errorf("%s started before %s of its closure was settled", n.d.Dist, name)
			}
			f.forced[n.d.Dist] = true
		}
	}
	var collected []string
	tree, err := gperlTreeName(n.d.Path)
	if err != nil {
		f.t.Fatal(err)
	}
	if !f.seen[n.d.Dist] {
		f.seen[n.d.Dist] = true
		if meta, ok := f.mymeta[n.d.Dist]; ok {
			writeFiles(f.t, f.s.treesDir, map[string]string{tree + "/MYMETA.json": meta})
		}
		collected = append(collected, n.d.Dist)
	}
	rec := &staticRecord{Dist: n.d.Dist, Closure: job.closure}
	rec.State, rec.Reason, _ = strings.Cut(f.result[n.d.Dist], ":")
	rec.BlockedBy = f.blockedBy[n.d.Dist]
	if rec.State == "" {
		rec.State = staticStatic
		if !n.target {
			rec.State = staticDependency
		}
	}
	return rec, collected
}

func TestStaticSchedule(t *testing.T) {
	archives := t.TempDir()
	mk := func(name string, dependents int, needs ...string) dist {
		d := dist{Dist: name, Release: name + "-1", Path: "A/AU/AUTHOR/" + name + "-1.tar.gz", Modules: []string{name}, DependentsTotal: dependents}
		for _, n := range needs {
			d.Requires = append(d.Requires, requirement{Module: n, Phase: "runtime", Relationship: "requires", Dist: n})
		}
		writeFiles(t, archives, map[string]string{"archives/" + d.Path: distArchive(t, d.Release+"/Makefile.PL")})
		return d
	}
	all := []dist{
		mk("App", 1, "Lib"), // Lib's configure step adds Extra
		mk("Lib", 3),
		mk("Extra", 0),
		mk("Broken", 2),
		mk("UsesBroken", 0, "Broken"),
		mk("CycleA", 0, "CycleB"),
		mk("CycleB", 0),   // its configure step adds CycleA
		mk("Relinked", 0), // its configure step adds Late
		mk("Late", 0),
		mk("Bad", 0, "BadName"),
		{Dist: "BadName", Release: "BadName-1.0-patched", Path: "A/AU/AUTHOR/BadName-1.0-patched.tar.gz", Modules: []string{"BadName"}},
	}
	s := &staticRunner{
		opts: staticOptions{dataDir: archives, jobs: 1}, dists: map[string]*dist{}, byModule: map[string]string{},
		versions: map[string]string{}, provided: map[string]string{}, targets: map[string]bool{},
		previous: map[string]*staticRecord{}, nodes: map[string]*staticNode{}, waiters: map[string][]*staticNode{},
		requirements: map[string]*distRequirements{}, verified: map[string]bool{}, testRequirements: map[string]*distRequirements{},
		treesDir: t.TempDir(),
	}
	for i := range all {
		s.dists[all[i].Dist] = &all[i]
		s.byModule[all[i].Dist] = all[i].Dist
	}
	openRecords(t, s)
	results := s.results.Name()
	targets := []string{"App", "Lib", "UsesBroken", "CycleA", "CycleB", "Relinked", "Bad"}
	for _, name := range targets {
		s.targets[name] = true
	}
	f := &fakeStatic{
		t: t, s: s, result: map[string]string{"Broken": staticDependency + ":" + reasonDistBuildFailed},
		mymeta: map[string]string{
			"Lib":      `{"meta-spec":{"version":2},"prereqs":{"runtime":{"requires":{"Extra":"0"}}}}`,
			"CycleB":   `{"meta-spec":{"version":2},"prereqs":{"runtime":{"requires":{"CycleA":"0"}}}}`,
			"Relinked": `{"meta-spec":{"version":2},"prereqs":{"runtime":{"requires":{"Late":"0"}}}}`,
		},
		starts: map[string][]string{}, forced: map[string]bool{}, seen: map[string]bool{},
	}
	for _, name := range targets {
		s.ensureNode(name)
	}
	if err := s.schedule(f.attempt); err != nil {
		t.Fatal(err)
	}
	recs := map[string]*staticRecord{}
	for name, n := range s.nodes {
		recs[name] = n.rec
	}
	// Lib's MYMETA.json asked for Extra: Lib was linked again once Extra
	// was built, and App, which waited for Lib, carried Extra from the start.
	if !reflect.DeepEqual(f.starts["Lib"], []string{"Lib", "Extra,Lib"}) {
		t.Errorf("Lib started with %q", f.starts["Lib"])
	}
	if !reflect.DeepEqual(f.starts["App"], []string{"App,Extra,Lib"}) {
		t.Errorf("App started with %q", f.starts["App"])
	}
	if recs["Extra"].State != staticDependency {
		t.Errorf("Extra: %+v", recs["Extra"])
	}
	if r := recs["UsesBroken"]; r.State != staticBlocked || !reflect.DeepEqual(r.BlockedBy, []string{"Broken"}) {
		t.Errorf("UsesBroken: %+v", r)
	}
	if _, ran := f.starts["UsesBroken"]; ran {
		t.Errorf("a blocked distribution's container ran")
	}
	// CycleA needs CycleB by its metadata; CycleB's configure step asks for
	// CycleA: CycleB was linked again with CycleA, which waited for CycleB.
	if !reflect.DeepEqual(f.starts["CycleB"], []string{"CycleB", "CycleA,CycleB"}) || len(f.starts["CycleA"]) != 1 {
		t.Errorf("the cycle: CycleA %q, CycleB %q", f.starts["CycleA"], f.starts["CycleB"])
	}
	if r := recs["BadName"]; r.State != staticDependency || r.Reason != reasonArchiveNotSupported {
		t.Errorf("BadName: %+v", r)
	}
	if r := recs["Bad"]; r.State != staticBlocked || !reflect.DeepEqual(r.BlockedBy, []string{"BadName"}) {
		t.Errorf("Bad: %+v", r)
	}
	if !reflect.DeepEqual(f.starts["Relinked"], []string{"Relinked", "Late,Relinked"}) {
		t.Errorf("Relinked started with %q", f.starts["Relinked"])
	}
	for _, name := range f.order {
		if recs[name] == nil {
			t.Errorf("%s was never settled", name)
		}
	}
	rows, err := readJSONLines[staticRecord](results)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(s.nodes) {
		t.Errorf("%d records written for %d distributions", len(rows), len(s.nodes))
	}
	if i, j := slices.Index(f.order, "Extra"), slices.Index(f.order, "App"); i < 0 || j < 0 || i > j {
		t.Errorf("Extra (%d) did not run before App (%d): %v", i, j, f.order)
	}
}

// openRecords gives the runner a records file the test closes.
func openRecords(t *testing.T, s *staticRunner) {
	t.Helper()
	var err error
	if s.results, err = os.Create(filepath.Join(t.TempDir(), "static.jsonl")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.results.Close(); err != nil {
			t.Error(err)
		}
	})
}

func TestStaticForcesAStandstill(t *testing.T) {
	// X and Y need each other by their metadata: neither can wait for the
	// other, so the first by name starts and builds both. When Y fails in
	// X's container, X is blocked by Y, and Y's own container tells why Y
	// fails (it is not blocked by X in turn).
	for _, tc := range []struct {
		name              string
		result            map[string]string
		blockedBy         map[string][]string
		stateX, stateY    string
		reasonY           string
		blockedX, blocked []string
	}{
		{name: "both build", result: map[string]string{}, stateX: staticStatic, stateY: staticStatic},
		{
			name:   "Y fails",
			result: map[string]string{"X": staticBlocked, "Y": staticNotStatic + ":" + reasonDistBuildFailed}, blockedBy: map[string][]string{"X": {"Y"}},
			stateX: staticBlocked, stateY: staticNotStatic, reasonY: reasonDistBuildFailed, blockedX: []string{"Y"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archives := t.TempDir()
			all := []dist{
				{Dist: "X", Release: "X-1", Path: "A/AU/AUTHOR/X-1.tar.gz", Modules: []string{"X"}, Requires: []requirement{{Module: "Y", Phase: "runtime", Relationship: "requires", Dist: "Y"}}},
				{Dist: "Y", Release: "Y-1", Path: "A/AU/AUTHOR/Y-1.tar.gz", Modules: []string{"Y"}, Requires: []requirement{{Module: "X", Phase: "runtime", Relationship: "requires", Dist: "X"}}},
			}
			writeFiles(t, archives, map[string]string{"archives/A/AU/AUTHOR/X-1.tar.gz": distArchive(t, "X-1/Makefile.PL"), "archives/A/AU/AUTHOR/Y-1.tar.gz": distArchive(t, "Y-1/Makefile.PL")})
			s := &staticRunner{
				opts: staticOptions{dataDir: archives, jobs: 1}, dists: map[string]*dist{"X": &all[0], "Y": &all[1]},
				byModule: map[string]string{"X": "X", "Y": "Y"}, versions: map[string]string{}, provided: map[string]string{},
				targets: map[string]bool{"X": true, "Y": true}, previous: map[string]*staticRecord{},
				nodes: map[string]*staticNode{}, waiters: map[string][]*staticNode{}, requirements: map[string]*distRequirements{}, testRequirements: map[string]*distRequirements{},
				verified: map[string]bool{}, treesDir: t.TempDir(),
			}
			openRecords(t, s)
			f := &fakeStatic{t: t, s: s, result: tc.result, blockedBy: tc.blockedBy, mymeta: map[string]string{},
				starts: map[string][]string{}, forced: map[string]bool{}, seen: map[string]bool{}}
			s.ensureNode("X")
			s.ensureNode("Y")
			if err := s.schedule(f.attempt); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(f.order, []string{"X", "Y"}) || !f.forced["X"] || f.forced["Y"] {
				t.Errorf("order %v, forced %v", f.order, f.forced)
			}
			x, y := s.nodes["X"].rec, s.nodes["Y"].rec
			if x.State != tc.stateX || !reflect.DeepEqual(x.BlockedBy, tc.blockedX) || y.State != tc.stateY || y.Reason != tc.reasonY || y.BlockedBy != nil {
				t.Errorf("X %+v, Y %+v", x, y)
			}
		})
	}
}

func TestStaticCategories(t *testing.T) {
	for _, tc := range []struct {
		rec  *staticRecord
		want string
	}{
		{nil, ""},
		{&staticRecord{State: staticStatic}, "static"},
		{&staticRecord{State: staticStatic, Reason: reasonBundlesSystemLibraries}, "static:" + reasonBundlesSystemLibraries},
		{&staticRecord{State: staticNotStatic, Reason: reasonNeedsSharedLibrary}, "not_static:" + reasonNeedsSharedLibrary},
		{&staticRecord{State: staticBlocked}, "blocked"},
		{&staticRecord{State: staticDependency}, ""},
	} {
		if got := staticCategoryOf(tc.rec); got != tc.want {
			t.Errorf("staticCategoryOf(%+v) = %q, want %q", tc.rec, got, tc.want)
		}
	}
	ids := map[string]bool{}
	for _, c := range staticCategories() {
		if ids[c.ID] || c.Title == "" || strings.HasSuffix(c.Title, ": ") {
			t.Errorf("category %+v", c)
		}
		ids[c.ID] = true
	}
	for reason := range staticReasonTitles {
		if reason != reasonBundlesSystemLibraries && !ids["not_static:"+reason] {
			t.Errorf("reason %s has no category", reason)
		}
	}
}

func TestStaticLogOf(t *testing.T) {
	for _, tc := range []struct {
		state, reason, want string
	}{
		{staticNotStatic, reasonDistBuildFailed, "modules.log"},
		{staticDependency, reasonDistBuildFailed, "modules.log"},
		{staticNotStatic, reasonNeedsSharedLibrary, "link.log"},
		{staticNotStatic, reasonLoadFailed, "run_empty.log"},
		{staticNotStatic, reasonNotFullyStatic, ""},
		{staticDriverFailed, "", "container.log"},
		{staticStatic, "", ""},
		{staticBlocked, "", ""},
	} {
		if got := staticLogOf(&staticRecord{State: tc.state, Reason: tc.reason}); got != tc.want {
			t.Errorf("staticLogOf(%s %s) = %q, want %q", tc.state, tc.reason, got, tc.want)
		}
	}
}

func TestFirstArchives(t *testing.T) {
	got := firstArchives([]string{"/usr/lib/gcc/aarch64-linux-gnu/12", "/usr/lib/aarch64-linux-gnu"},
		[]string{"/usr/lib/aarch64-linux-gnu/libz.a", "/usr/lib/gcc/aarch64-linux-gnu/12/libgcc.a", "/usr/lib/aarch64-linux-gnu/libgcc.a"})
	want := map[string]string{"libz.a": "/usr/lib/aarch64-linux-gnu/libz.a", "libgcc.a": "/usr/lib/gcc/aarch64-linux-gnu/12/libgcc.a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("firstArchives = %v", got)
	}
}
