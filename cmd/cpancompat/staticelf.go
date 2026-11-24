package main

// What the static build phase reads from the files gperl makes, rather than
// from what its logs say: the ELF headers of a binary, and of each native
// module of a program its shared object (the libraries it names and the
// symbols it defines and uses) and its .ldflags (the libraries its
// distribution's link asked for).

import (
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// imageModDir is gperl's module directory in the build containers.
const imageModDir = "/opt/gperl/mod"

// cRuntimeLibraries are the libraries that are not external: the ones the
// perl executable itself links (perl's perllibs: pthread, dl, m, crypt,
// util, c, and rt), glibc's dynamic loader (which libc.so names for a module
// with thread-local storage), and the C compiler's runtime (gcc, gcc_s,
// stdc++).
var cRuntimeLibraries = map[string]bool{
	"c": true, "m": true, "pthread": true, "dl": true, "crypt": true, "util": true, "rt": true,
	"ld-linux-aarch64": true, "ld-linux-x86-64": true,
	"gcc": true, "gcc_s": true, "stdc++": true,
}

// runtimeLibraryFiles are the C runtime's shared libraries whose symbols
// tell a function of the C library from one of a native module, by the
// library name they stand for.
var runtimeLibraryFiles = map[string]string{"c": "libc.so.6", "m": "libm.so.6", "crypt": "libcrypt.so.1"}

// runtimeSymbols maps each library of runtimeLibraryFiles to the symbols it
// defines.
type runtimeSymbols map[string]map[string]bool

// readRuntimeSymbols reads the dynamic symbols of the C runtime's shared
// libraries in dir.
func readRuntimeSymbols(dir string) (runtimeSymbols, error) {
	rt := runtimeSymbols{}
	for lib, file := range runtimeLibraryFiles {
		f, err := elf.Open(filepath.Join(dir, file))
		if err != nil {
			return nil, err
		}
		global, weak, _, err := dynamicSymbols(f)
		for sym := range weak {
			global[sym] = true
		}
		defined := global
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		rt[lib] = defined
	}
	return rt, nil
}

// dynamicSymbols returns the symbols an ELF object defines, global and
// weak, and the ones it needs from elsewhere (a weak reference needs
// nothing).
func dynamicSymbols(f *elf.File) (global, weak map[string]bool, undefined []string, err error) {
	syms, err := f.DynamicSymbols()
	if err != nil && !errors.Is(err, elf.ErrNoSymbols) {
		return nil, nil, nil, err
	}
	global, weak = map[string]bool{}, map[string]bool{}
	for _, s := range syms {
		bind := elf.ST_BIND(s.Info)
		switch {
		case s.Name == "" || bind == elf.STB_LOCAL:
		case s.Section != elf.SHN_UNDEF && bind == elf.STB_WEAK:
			weak[s.Name] = true
		case s.Section != elf.SHN_UNDEF:
			global[s.Name] = true
		case bind == elf.STB_GLOBAL:
			undefined = append(undefined, s.Name)
		}
	}
	sort.Strings(undefined)
	return global, weak, undefined, nil
}

// binaryOf reads what makes the binary at name depend on its environment.
func binaryOf(name string) (binaryInfo, error) {
	var info binaryInfo
	st, err := os.Stat(name)
	if err != nil {
		return info, err
	}
	info.Bytes = st.Size()
	f, err := elf.Open(name)
	if err != nil {
		return info, err
	}
	err = readBinary(f, &info)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return info, fmt.Errorf("%s: %w", name, err)
	}
	return info, nil
}

func readBinary(f *elf.File, info *binaryInfo) error {
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		raw, err := io.ReadAll(p.Open())
		if err != nil {
			return fmt.Errorf("PT_INTERP: %w", err)
		}
		info.Interp = strings.TrimRight(string(raw), "\x00")
	}
	needed, err := f.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("DT_NEEDED: %w", err)
	}
	info.Needed = append([]string{}, needed...)
	return nil
}

// moduleELF is one native module of a program, as its files tell.
type moduleELF struct {
	pkg, dist string
	// ldflags are its .ldflags: the library flags of its link, in order.
	ldflags []string
	// needed are the shared libraries its .so names (DT_NEEDED).
	needed    []string
	defined   map[string]bool
	undefined []string
	// archives are the static archives in the directories its tree's
	// .ldflags name with -L, by path.
	archives map[string]bool
}

// closureModules reads the native modules of the closure's distributions
// from the run's trees, sorted by distribution and package.
func (s *staticRunner) closureModules(closure []string) ([]moduleELF, error) {
	var out []moduleELF
	for _, name := range closure {
		tree, err := gperlTreeName(s.dists[name].Path)
		if err != nil {
			return nil, err
		}
		mods, err := treeModules(filepath.Join(s.treesDir, tree), name)
		if err != nil {
			return nil, err
		}
		out = append(out, mods...)
	}
	return out, nil
}

// treeModules reads the native modules collected from one tree (dir): every
// abi<N>/<target>/auto/<Module/Path>/<Name>.so with its .ldflags, whose
// package is its directory below auto/ (gperl's nativeModule.perlPackage).
func treeModules(dir, dist string) ([]moduleELF, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("the run has not collected the tree of %s: %w", dist, err)
	}
	archives := map[string]bool{}
	raw, err := os.ReadFile(filepath.Join(dir, "archives.txt"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, a := range strings.Fields(string(raw)) {
		archives[a] = true
	}
	autos, err := filepath.Glob(filepath.Join(dir, "abi*", "*", "auto"))
	if err != nil {
		return nil, err
	}
	var out []moduleELF
	for _, auto := range autos {
		err := filepath.WalkDir(auto, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || filepath.Ext(p) != ".so" {
				return err
			}
			rel, err := filepath.Rel(auto, filepath.Dir(p))
			if err != nil {
				return err
			}
			m := moduleELF{pkg: strings.ReplaceAll(filepath.ToSlash(rel), "/", "::"), dist: dist, archives: archives}
			flags, err := os.ReadFile(strings.TrimSuffix(p, ".so") + ".ldflags")
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			m.ldflags = strings.Fields(string(flags))
			f, err := elf.Open(p)
			if err != nil {
				return err
			}
			m.needed, err = f.ImportedLibraries()
			if err == nil {
				// A weak definition (a C++ template's, say) is one any
				// object may carry: it tells nothing of who lends to whom.
				m.defined, _, m.undefined, err = dynamicSymbols(f)
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			out = append(out, m)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pkg < out[j].pkg })
	return out, nil
}

// libraryName returns the name a library is linked by (-l<name>) from the
// name of one of its files: libssl.so.3 and libssl.a are "ssl".
func libraryName(file string) string {
	base := strings.TrimPrefix(path.Base(file), "lib")
	if i := strings.Index(base, ".so"); i >= 0 {
		return base[:i]
	}
	return strings.TrimSuffix(base, ".a")
}

// linkedLibraries returns the libraries a module links, each with the file
// a static link takes it from: -l<name> is lib<name>.a, -l:<file> is that
// file, and a library its .so names is lib<name>.a too.
func (m *moduleELF) linkedLibraries() [][2]string {
	var out [][2]string
	for _, f := range m.ldflags {
		switch {
		case strings.HasPrefix(f, "-l:"):
			out = append(out, [2]string{libraryName(f[3:]), f[3:]})
		case strings.HasPrefix(f, "-l"):
			out = append(out, [2]string{f[2:], "lib" + f[2:] + ".a"})
		}
	}
	for _, n := range m.needed {
		out = append(out, [2]string{libraryName(n), "lib" + libraryName(n) + ".a"})
	}
	return out
}

// links reports whether the module's .ldflags names the library.
func (m *moduleELF) links(lib string) bool {
	return slices.Contains(m.ldflags, "-l"+lib) || slices.Contains(m.ldflags, "-l:lib"+lib+".a")
}

// staticArchive returns the path of the file the linker takes for a static
// link of the module: the first directory its .ldflags names with -L that
// has it, or else the compiler's (system); "" when none has it.
func (m *moduleELF) staticArchive(file string, system map[string]string) string {
	if !strings.HasSuffix(file, ".a") {
		return ""
	}
	for _, f := range m.ldflags {
		if dir, ok := strings.CutPrefix(f, "-L"); ok && path.IsAbs(dir) {
			if p := path.Join(dir, file); m.archives[p] {
				return p
			}
		}
	}
	return system[file]
}

// externalLibraries returns the libraries outside the C runtime that the
// modules link, once per library and distribution, with where the static
// link finds each.
func externalLibraries(mods []moduleELF, system map[string]string) []externalLib {
	seen := map[[2]string]bool{}
	var out []externalLib
	for i := range mods {
		m := &mods[i]
		for _, lib := range m.linkedLibraries() {
			name, file := lib[0], lib[1]
			if cRuntimeLibraries[name] || seen[[2]string{name, m.dist}] {
				continue
			}
			seen[[2]string{name, m.dist}] = true
			e := externalLib{Name: name, Dist: m.dist}
			if p := m.staticArchive(file, system); p != "" {
				e.StaticArchive, e.From = true, "system"
				if strings.HasPrefix(p, imageModDir+"/") {
					e.From = "module_directory"
				}
			}
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Dist < out[j].Dist
	})
	return out
}

// isSharedLibraryPath reports whether an .ldflags entry names a shared
// library by its path (gperl's linker wrapper keeps those), which gperl
// does not pass on to a static link: only -l, -L and -pthread go there.
func isSharedLibraryPath(flag string) bool {
	return !strings.HasPrefix(flag, "-") &&
		(strings.HasSuffix(flag, ".so") || strings.HasSuffix(flag, ".dylib") || strings.HasSuffix(flag, ".tbd"))
}

// linkFailure says why the link of own's program failed, from the files of
// its native modules, and which other distributions' modules it is found
// in. The first that holds: a module names a shared library by its path; a
// library has no static archive; a module uses a symbol only another module
// defines (gperl's static archives keep nothing visible but the entry
// table); a module uses a function of a part of the C library it did not
// link (libm, libcrypt). Otherwise the link failed for a reason only its
// log tells.
func linkFailure(own string, mods []moduleELF, rt runtimeSymbols, libs []externalLib) (string, []string) {
	var found []string
	for _, m := range mods {
		if slices.ContainsFunc(m.ldflags, isSharedLibraryPath) {
			found = append(found, m.dist)
		}
	}
	if len(found) > 0 {
		return reasonSharedLibraryPathDropped, others(own, found)
	}
	for _, l := range libs {
		if !l.StaticArchive {
			found = append(found, l.Dist)
		}
	}
	if len(found) > 0 {
		return reasonNeedsSharedLibrary, others(own, found)
	}
	definedIn := map[string][]int{}
	for i, m := range mods {
		for sym := range m.defined {
			definedIn[sym] = append(definedIn[sym], i)
		}
	}
	inRuntime := func(sym string) bool { return rt["c"][sym] || rt["m"][sym] || rt["crypt"][sym] }
	for i, m := range mods {
		for _, sym := range m.undefined {
			if inRuntime(sym) {
				continue
			}
			for _, j := range definedIn[sym] {
				if j != i {
					found = append(found, m.dist, mods[j].dist)
				}
			}
		}
	}
	if len(found) > 0 {
		return reasonCrossModuleSymbols, others(own, found)
	}
	for _, m := range mods {
		for _, sym := range m.undefined {
			if !rt["c"][sym] && (rt["m"][sym] && !m.links("m") || rt["crypt"][sym] && !m.links("crypt")) {
				found = append(found, m.dist)
				break
			}
		}
	}
	if len(found) > 0 {
		return reasonCRuntimeNotLinked, others(own, found)
	}
	return reasonLinkFailed, nil
}

// others returns, sorted and once each, the names other than own.
func others(own string, names []string) []string {
	out := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == own })
	slices.Sort(out)
	return slices.Compact(out)
}
