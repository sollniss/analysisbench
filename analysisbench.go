package analysisbench

import (
	"fmt"
	"go/types"
	"log"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"
)

// TestData returns the effective filename of the program's "testdata" directory.
// This function may be overridden by projects using an alternative build system (such as Blaze)
// that does not run a test in its package directory.
var TestData = func() string {
	testdata, err := filepath.Abs("testdata")
	if err != nil {
		log.Fatal(err)
	}
	return testdata
}

// objectFactKey identifies a (object, fact-type) pair.
type objectFactKey struct {
	obj types.Object
	typ reflect.Type
}

// packageFactKey identifies a (package, fact-type) pair.
type packageFactKey struct {
	pkg *types.Package
	typ reflect.Type
}

// passTemplate holds pre-computed state for a single root package,
// from which fresh [analysis.Pass] values can be created on each benchmark iteration.
type passTemplate struct {
	pkg          *packages.Package
	module       *analysis.Module
	resultOf     map[*analysis.Analyzer]any
	objectFacts  map[objectFactKey]analysis.Fact
	packageFacts map[packageFactKey]analysis.Fact
}

// nopReport is a no-op diagnostic handler used in the benchmark loop
// to avoid unnecessary allocation overhead from collecting diagnostics.
var nopReport = func(analysis.Diagnostic) {}

// buildPass creates a fresh [analysis.Pass] for one benchmark iteration.
//
// The pre-computed fact maps are cloned so that facts exported during
// the iteration are visible to subsequent imports and AllFacts calls
// within the same iteration, but do not persist across iterations.
//
// report is called for each diagnostic the analyzer produces.
// Callers that do not need diagnostics may pass [nopReport].
//
// buildPass returns the pass and the cloned fact maps.
// The caller may inspect the fact maps after [analysis.Analyzer.Run] returns.
func (pt *passTemplate) buildPass(
	a *analysis.Analyzer,
	report func(analysis.Diagnostic),
) (
	pass *analysis.Pass,
	objectFacts map[objectFactKey]analysis.Fact,
	packageFacts map[packageFactKey]analysis.Fact,
) {
	objectFacts = maps.Clone(pt.objectFacts)
	packageFacts = maps.Clone(pt.packageFacts)

	pass = &analysis.Pass{
		Analyzer:     a,
		Fset:         pt.pkg.Fset,
		Files:        pt.pkg.Syntax,
		OtherFiles:   pt.pkg.OtherFiles,
		IgnoredFiles: pt.pkg.IgnoredFiles,
		Pkg:          pt.pkg.Types,
		TypesInfo:    pt.pkg.TypesInfo,
		TypesSizes:   pt.pkg.TypesSizes,
		TypeErrors:   pt.pkg.TypeErrors,
		Module:       pt.module,
		ResultOf:     pt.resultOf,
		Report:       report,
		ReadFile:     os.ReadFile,
	}

	pass.ImportObjectFact = func(obj types.Object, ptr analysis.Fact) bool {
		if obj == nil {
			panic("nil object")
		}
		key := objectFactKey{obj, reflect.TypeOf(ptr)}
		v, ok := objectFacts[key]
		if ok {
			reflect.ValueOf(ptr).Elem().Set(reflect.ValueOf(v).Elem())
		}
		return ok
	}

	pass.ExportObjectFact = func(obj types.Object, fact analysis.Fact) {
		if obj.Pkg() != pass.Pkg {
			panic(fmt.Sprintf("ExportObjectFact(%s, %T): object belongs to %s, not %s",
				obj, fact, obj.Pkg().Path(), pass.Pkg.Path()))
		}
		objectFacts[objectFactKey{obj, reflect.TypeOf(fact)}] = fact
	}

	pass.ImportPackageFact = func(pkg *types.Package, ptr analysis.Fact) bool {
		if pkg == nil {
			panic("nil package")
		}
		key := packageFactKey{pkg, reflect.TypeOf(ptr)}
		v, ok := packageFacts[key]
		if ok {
			reflect.ValueOf(ptr).Elem().Set(reflect.ValueOf(v).Elem())
		}
		return ok
	}

	pass.ExportPackageFact = func(fact analysis.Fact) {
		packageFacts[packageFactKey{pass.Pkg, reflect.TypeOf(fact)}] = fact
	}

	pass.AllObjectFacts = func() []analysis.ObjectFact {
		out := make([]analysis.ObjectFact, 0, len(objectFacts))
		for k, v := range objectFacts {
			out = append(out, analysis.ObjectFact{Object: k.obj, Fact: v})
		}
		return out
	}

	pass.AllPackageFacts = func() []analysis.PackageFact {
		out := make([]analysis.PackageFact, 0, len(packageFacts))
		for k, v := range packageFacts {
			out = append(out, analysis.PackageFact{Package: k.pkg, Fact: v})
		}
		return out
	}

	return pass, objectFacts, packageFacts
}

// precompute runs the full analysis once, returning one [passTemplate] per root package.
func precompute(tb testing.TB, a *analysis.Analyzer, pkgs []*packages.Package) []*passTemplate {
	tb.Helper()

	// Run the dependency analyzers as roots so that checker.Analyze preserves their Result values.
	// We key by *packages.Package to correctly distinguish test variants that share the same PkgPath.

	type pkgAnalyzerKey struct {
		pkg      *packages.Package
		analyzer *analysis.Analyzer
	}
	depResults := make(map[pkgAnalyzerKey]any)

	if len(a.Requires) > 0 {
		res, err := checker.Analyze(a.Requires, pkgs, nil)
		if err != nil {
			tb.Fatalf("pre-computing dependency analyzers: %s", err.Error())
		}
		for act := range res.All() {
			if act.Err != nil {
				tb.Fatalf("error in dependency analyzer %s on %s: %s",
					act.Analyzer.Name, act.Package.PkgPath, act.Err.Error())
			}
			depResults[pkgAnalyzerKey{act.Package, act.Analyzer}] = act.Result
		}
	}

	// When the analyzer declares FactTypes, run the full analysis
	// so that facts produced on dependency packages are propagated to root actions.
	// We snapshot the accumulated facts per root package.

	type factSnapshot struct {
		objectFacts  map[objectFactKey]analysis.Fact
		packageFacts map[packageFactKey]analysis.Fact
	}
	cachedFacts := make(map[*packages.Package]*factSnapshot)

	if len(a.FactTypes) > 0 {
		res, err := checker.Analyze([]*analysis.Analyzer{a}, pkgs, nil)
		if err != nil {
			tb.Fatalf("pre-computing facts: %s", err.Error())
		}
		for act := range res.All() {
			if act.Err != nil {
				tb.Fatalf("error in analyzer %s on %s: %s",
					act.Analyzer.Name, act.Package.PkgPath, act.Err.Error())
			}
			if act.Analyzer == a && act.IsRoot {
				fs := &factSnapshot{
					objectFacts:  make(map[objectFactKey]analysis.Fact),
					packageFacts: make(map[packageFactKey]analysis.Fact),
				}
				for _, of := range act.AllObjectFacts() {
					fs.objectFacts[objectFactKey{of.Object, reflect.TypeOf(of.Fact)}] = of.Fact
				}
				for _, pf := range act.AllPackageFacts() {
					fs.packageFacts[packageFactKey{pf.Package, reflect.TypeOf(pf.Fact)}] = pf.Fact
				}
				cachedFacts[act.Package] = fs
			}
		}
	}

	templates := make([]*passTemplate, 0, len(pkgs))
	for _, pkg := range pkgs {
		if pkg.IllTyped && !a.RunDespiteErrors {
			continue
		}

		module := &analysis.Module{}
		if mod := pkg.Module; mod != nil {
			module.Path = mod.Path
			module.Version = mod.Version
			module.GoVersion = mod.GoVersion
		}

		resultOf := make(map[*analysis.Analyzer]any, len(a.Requires))
		for _, req := range a.Requires {
			resultOf[req] = depResults[pkgAnalyzerKey{pkg, req}]
		}

		pt := &passTemplate{
			pkg:          pkg,
			module:       module,
			resultOf:     resultOf,
			objectFacts:  make(map[objectFactKey]analysis.Fact),
			packageFacts: make(map[packageFactKey]analysis.Fact),
		}

		if fs, ok := cachedFacts[pkg]; ok {
			pt.objectFacts = fs.objectFacts
			pt.packageFacts = fs.packageFacts
		}

		templates = append(templates, pt)
	}

	return templates
}

// WriteFiles creates a temporary GOPATH-style directory tree from filemap and
// returns the root directory. It mirrors analysistest.WriteFiles.
func WriteFiles(t testing.TB, filemap map[string]string) string {
	t.Helper()
	gopath := t.TempDir()
	for name, content := range filemap {
		filename := filepath.Join(gopath, "src", name)
		if err := os.MkdirAll(filepath.Dir(filename), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	return gopath
}

// Run benchmarks the analyzer on the packages
// using the same conventions as [analysistest.Run].
//
// Packages are loaded once before the benchmark loop begins so that
// only the analysis itself is measured.
// Dependency analyzers are also pre-computed once and
// only the target analyzer's [analysis.Analyzer.Run] function is invoked in the hot loop.
//
// If the analyzer declares [analysis.Analyzer.FactTypes], the full
// analysis is executed once so that the accumulated facts can be injected into
// each benchmarked pass.
//
// Run reports a fatal error to b if loading or analysis fails.
func Run(b *testing.B, dir string, a *analysis.Analyzer, patterns ...string) {
	b.Helper()

	pkgs, err := loadPackages(dir, patterns...)
	if err != nil {
		b.Fatalf("loading %s: %s", patterns, err.Error())
	}

	if !a.RunDespiteErrors {
		packages.Visit(pkgs, nil, func(pkg *packages.Package) {
			for _, err := range pkg.Errors {
				b.Log(err)
			}
		})
	}

	templates := precompute(b, a, pkgs)

	b.ResetTimer()
	for b.Loop() {
		for _, pt := range templates {
			pass, _, _ := pt.buildPass(a, nopReport)
			if _, err := a.Run(pass); err != nil {
				b.Fatalf("error analyzing %s: %s", pass.Pkg.Path(), err.Error())
			}
		}
	}
}

// RunPerPackage is like [Run] but creates a sub-benchmark for each
// root package, allowing per-package timing granularity.
//
// The sub-benchmarks are named by the package's import path.
func RunPerPackage(b *testing.B, dir string, a *analysis.Analyzer, patterns ...string) {
	b.Helper()

	pkgs, err := loadPackages(dir, patterns...)
	if err != nil {
		b.Fatalf("loading %s: %s", patterns, err.Error())
	}

	if !a.RunDespiteErrors {
		packages.Visit(pkgs, nil, func(pkg *packages.Package) {
			for _, err := range pkg.Errors {
				b.Log(err)
			}
		})
	}

	templates := precompute(b, a, pkgs)

	for _, pt := range templates {
		b.Run(pt.pkg.PkgPath, func(b *testing.B) {
			for b.Loop() {
				pass, _, _ := pt.buildPass(a, nopReport)
				if _, err := a.Run(pass); err != nil {
					b.Fatalf("error analyzing %s: %s", pass.Pkg.Path(), err.Error())
				}
			}
		})
	}
}

// loadPackages mirrors the unexported loadPackages in
// golang.org/x/tools/go/analysis/analysistest so that the same
// GOPATH-style and module-mode directory conventions are supported.
func loadPackages(dir string, patterns ...string) ([]*packages.Package, error) {
	env := []string{"GOPATH=" + dir, "GO111MODULE=off", "GOWORK=off"} // GOPATH mode

	// Undocumented module mode: if dir contains a go.mod, switch to module mode.
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		gowork := filepath.Join(dir, "go.work")
		if _, err := os.Stat(gowork); err != nil {
			gowork = "off"
		}
		env = []string{"GO111MODULE=on", "GOPROXY=off", "GOWORK=" + gowork}
	}

	mode := packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports |
		packages.NeedTypes | packages.NeedTypesSizes | packages.NeedSyntax | packages.NeedTypesInfo |
		packages.NeedDeps | packages.NeedModule
	cfg := &packages.Config{
		Mode:  mode,
		Dir:   dir,
		Tests: true,
		Env:   append(os.Environ(), env...),
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, err
	}

	// Fail fast if any named package couldn't be loaded at all.
	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			return nil, fmt.Errorf("failed to load %q: Errors=%v", pkg.PkgPath, pkg.Errors)
		}
	}

	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages matched %s", patterns)
	}
	return pkgs, nil
}
