package analysisbench

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/packages"
)

// diagString formats a diagnostic as "file:line: message" so that
// it can be compared across runs deterministically.
func diagString(fset *token.FileSet, d analysis.Diagnostic) string {
	posn := fset.Position(d.Pos)
	return fmt.Sprintf("%s:%d: %s", posn.Filename, posn.Line, d.Message)
}

type factEntry struct {
	pkgPath string   // package that was analyzed
	objects []string // "objName: factString" sorted
	pkgs    []string // "pkgPath: factString" sorted
}

type analysisResult struct {
	diags []string
	facts []factEntry
}

// runAnalysis runs checker.Analyze with the given analyzer on pkgs
// and returns sorted diagnostic strings and facts for all root actions.
func runAnalysis(t *testing.T, a *analysis.Analyzer, pkgs []*packages.Package) analysisResult {
	t.Helper()
	res, err := checker.Analyze([]*analysis.Analyzer{a}, pkgs, nil)
	if err != nil {
		t.Fatalf("Analyze(%s): %v", a.Name, err)
	}
	var result analysisResult
	for _, act := range res.Roots {
		if act.Err != nil {
			t.Fatalf("error analyzing %s: %v", act, act.Err)
		}
		for _, d := range act.Diagnostics {
			result.diags = append(result.diags, diagString(act.Package.Fset, d))
		}

		fe := factEntry{pkgPath: act.Package.PkgPath}
		for _, of := range act.AllObjectFacts() {
			if of.Object.Pkg() == act.Package.Types {
				fe.objects = append(fe.objects, fmt.Sprintf("%s: %s", of.Object.Name(), of.Fact))
			}
		}
		sort.Strings(fe.objects)
		for _, pf := range act.AllPackageFacts() {
			if pf.Package == act.Package.Types {
				fe.pkgs = append(fe.pkgs, fmt.Sprintf("%s: %s", pf.Package.Path(), pf.Fact))
			}
		}
		sort.Strings(fe.pkgs)
		result.facts = append(result.facts, fe)
	}
	sort.Strings(result.diags)
	sort.Slice(result.facts, func(i, j int) bool {
		return result.facts[i].pkgPath < result.facts[j].pkgPath
	})
	return result
}

// runViaTemplates run the analyzer using [precompute] and [passTemplate.buildPass] and
// collects results in the same format as [runAnalysis] for comparison.
func runViaTemplates(t *testing.T, a *analysis.Analyzer, pkgs []*packages.Package) analysisResult {
	t.Helper()
	templates := precompute(t, a, pkgs)

	var result analysisResult
	for _, pt := range templates {
		var diags []analysis.Diagnostic
		pass, objFacts, pkgFacts := pt.buildPass(a, func(d analysis.Diagnostic) {
			diags = append(diags, d)
		})

		if _, err := a.Run(pass); err != nil {
			t.Fatalf("error analyzing %s: %v", pass.Pkg.Path(), err)
		}

		for _, d := range diags {
			result.diags = append(result.diags, diagString(pt.pkg.Fset, d))
		}

		fe := factEntry{pkgPath: pt.pkg.PkgPath}
		for k, v := range objFacts {
			if k.obj.Pkg() == pt.pkg.Types {
				fe.objects = append(fe.objects, fmt.Sprintf("%s: %s", k.obj.Name(), v))
			}
		}
		sort.Strings(fe.objects)
		for k, v := range pkgFacts {
			if k.pkg == pt.pkg.Types {
				fe.pkgs = append(fe.pkgs, fmt.Sprintf("%s: %s", k.pkg.Path(), v))
			}
		}
		sort.Strings(fe.pkgs)
		result.facts = append(result.facts, fe)
	}

	sort.Strings(result.diags)
	sort.Slice(result.facts, func(i, j int) bool {
		return result.facts[i].pkgPath < result.facts[j].pkgPath
	})
	return result
}

// compareResults compares the diagnostics and facts from the original
// analyzer run against the template-based run and reports mismatches.
func compareResults(t *testing.T, label string, want, got analysisResult) {
	t.Helper()

	if !slices.Equal(want.diags, got.diags) {
		t.Errorf("%s: diagnostics differ\nwant:\n  %s\ngot:\n  %s",
			label,
			strings.Join(want.diags, "\n  "),
			strings.Join(got.diags, "\n  "))
	}

	if len(want.facts) != len(got.facts) {
		t.Errorf("%s: fact entry count differs: want %d, got %d",
			label, len(want.facts), len(got.facts))
		return
	}

	for i := range want.facts {
		wf, gf := want.facts[i], got.facts[i]
		if wf.pkgPath != gf.pkgPath {
			t.Errorf("%s: fact entry %d: package path differs: want %s, got %s",
				label, i, wf.pkgPath, gf.pkgPath)
			continue
		}
		if !slices.Equal(wf.objects, gf.objects) {
			t.Errorf("%s: object facts for %s differ\nwant:\n  %s\ngot:\n  %s",
				label, wf.pkgPath,
				strings.Join(wf.objects, "\n  "),
				strings.Join(gf.objects, "\n  "))
		}
		if !slices.Equal(wf.pkgs, gf.pkgs) {
			t.Errorf("%s: package facts for %s differ\nwant:\n  %s\ngot:\n  %s",
				label, wf.pkgPath,
				strings.Join(wf.pkgs, "\n  "),
				strings.Join(gf.pkgs, "\n  "))
		}
	}
}

// runAndCompare loads packages, runs both the original analyzer and
// the template-based pass, and asserts that the results are identical.
func runAndCompare(t *testing.T, a *analysis.Analyzer, dir string, patterns ...string) {
	t.Helper()

	pkgs, err := loadPackages(dir, patterns...)
	if err != nil {
		t.Fatalf("loading %s: %v", patterns, err)
	}

	original := runAnalysis(t, a, pkgs)
	templated := runViaTemplates(t, a, pkgs)

	compareResults(t, a.Name, original, templated)
}

func TestBasic(t *testing.T) {
	noopAnalyzer := &analysis.Analyzer{
		Name: "noop",
		Doc:  "does nothing",
		Run: func(pass *analysis.Pass) (any, error) {
			return nil, nil
		},
	}

	t.Run("NoOutput", func(t *testing.T) {
		dir := WriteFiles(t, map[string]string{
			"a/a.go": `package a

func Foo() {}
`,
		})
		runAndCompare(t, noopAnalyzer, dir, "./...")
	})

	t.Run("EmptyPackage", func(t *testing.T) {
		dir := WriteFiles(t, map[string]string{
			"a/a.go": `package a`,
		})
		runAndCompare(t, noopAnalyzer, dir, "./...")
	})

	t.Run("DespiteErrors", func(t *testing.T) {
		dir := WriteFiles(t, map[string]string{
			"a/a.go": `package a

func Foo() int {
		return "not an int" // type error
}
`,
		})
		despiteErrorsAnalyzer := &analysis.Analyzer{
			Name:             "despiteerrors",
			Doc:              "runs despite type errors",
			RunDespiteErrors: true,
			Run: func(pass *analysis.Pass) (any, error) {
				for _, f := range pass.Files {
					pass.Reportf(f.Package, "analyzed despite errors")
				}
				return nil, nil
			},
		}
		runAndCompare(t, despiteErrorsAnalyzer, dir, "a")
	})
}

func TestDiagnostics(t *testing.T) {
	diagAnalyzer := &analysis.Analyzer{
		Name: "diag",
		Doc:  "reports one diagnostic per declaration",
		Run: func(pass *analysis.Pass) (any, error) {
			for _, f := range pass.Files {
				for _, d := range f.Decls {
					pass.Reportf(d.Pos(), "diagnostic in %s", pass.Pkg.Path())
				}
			}
			return nil, nil
		},
	}

	t.Run("None", func(t *testing.T) {
		dir := WriteFiles(t, map[string]string{
			"a/a.go": `package a`,
		})
		runAndCompare(t, diagAnalyzer, dir, "./...")
	})

	t.Run("MultiplePerPackage", func(t *testing.T) {
		dir := WriteFiles(t, map[string]string{
			"a/a.go": `package a

func Foo() {}
`,
			"a/b.go": `package a

func Bar() {}
`,
			"b/a.go": `package b

func Foo() {}
func Bar() {}
`,
		})
		runAndCompare(t, diagAnalyzer, dir, "./...")
	})
}

func TestRequires(t *testing.T) {
	dir := WriteFiles(t, map[string]string{
		"a/a.go": `package a

func Foo() {}
`,
		"a/b.go": `package a

func Bar() {}
`,
		"b/a.go": `package b

func Foo() {}
func Bar() {}
`,
	})

	t.Run("Inspect", func(t *testing.T) {
		requiresInspectAnalyzer := &analysis.Analyzer{
			Name: "requiresinspect",
			Doc:  "reports one diagnostic per function declaration via inspect.Analyzer",
			Requires: []*analysis.Analyzer{
				inspect.Analyzer,
			},
			Run: func(pass *analysis.Pass) (any, error) {
				insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
				for cur := range insp.Root().Preorder((*ast.FuncDecl)(nil)) {
					fn := cur.Node().(*ast.FuncDecl)
					pass.Reportf(fn.Name.Pos(), "func %s", fn.Name.Name)
				}
				return nil, nil
			},
		}
		runAndCompare(t, requiresInspectAnalyzer, dir, "./...")
	})

	t.Run("Multiple", func(t *testing.T) {
		resultAnalyzer := &analysis.Analyzer{
			Name:       "result",
			Doc:        "returns the number of files as result",
			ResultType: reflect.TypeFor[int](),
			Run: func(pass *analysis.Pass) (any, error) {
				return len(pass.Files), nil
			},
		}
		requiresMultipleAnalyzer := &analysis.Analyzer{
			Name: "requiresinspect",
			Doc:  "reports one diagnostic per function declaration via inspect.Analyzer",
			Requires: []*analysis.Analyzer{
				resultAnalyzer,
				inspect.Analyzer,
			},
			Run: func(pass *analysis.Pass) (any, error) {
				insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
				for cur := range insp.Root().Preorder((*ast.FuncDecl)(nil)) {
					fn := cur.Node().(*ast.FuncDecl)
					pass.Reportf(fn.Name.Pos(), "func %s", fn.Name.Name)
				}
				fileCount := pass.ResultOf[resultAnalyzer].(int)
				pass.Reportf(pass.Files[0].Package, "files %d", fileCount)
				return nil, nil
			},
		}
		runAndCompare(t, requiresMultipleAnalyzer, dir, "./...")
	})

	t.Run("Chain", func(t *testing.T) {
		result1Analyzer := &analysis.Analyzer{
			Name:       "result1",
			Doc:        "returns the number of files as result",
			ResultType: reflect.TypeFor[int](),
			Run: func(pass *analysis.Pass) (any, error) {
				return len(pass.Files), nil
			},
		}
		result2Analyzer := &analysis.Analyzer{
			Name: "result2",
			Doc:  "returns the number of files plus the number of declarations as result",
			Requires: []*analysis.Analyzer{
				result1Analyzer,
			},
			ResultType: reflect.TypeFor[int](),
			Run: func(pass *analysis.Pass) (any, error) {
				count := pass.ResultOf[result1Analyzer].(int)
				for _, f := range pass.Files {
					count += len(f.Decls)
				}
				return count, nil
			},
		}
		requiresAnalyzer := &analysis.Analyzer{
			Name: "result2",
			Doc:  "returns the number of files plus the number of declarations as result",
			Requires: []*analysis.Analyzer{
				result2Analyzer,
			},
			Run: func(pass *analysis.Pass) (any, error) {
				count := pass.ResultOf[result2Analyzer].(int)
				pass.Reportf(pass.Files[0].Package, "result %d", count)
				return nil, nil
			},
		}
		runAndCompare(t, requiresAnalyzer, dir, "./...")
	})
}

// testFact is a fact type used by the fact-producing analyzers.
type testFact struct{ Name string }

func (*testFact) AFact()           {}
func (f *testFact) String() string { return fmt.Sprintf("testFact:%s", f.Name) }

func TestFacts(t *testing.T) {
	dir := WriteFiles(t, map[string]string{
		"a/a.go": `package a

type T1 int
func F1() {}
`,
		"a/b.go": `package a

type T2 int
func F2() {}
`,
		"b/a.go": `package b

type T1 int
type T2 int
func F1() {}
func F2() {}
`,
		"c/a.go": `package c

import "a"
import "b"

var At1 a.T1
var At2 a.T2
var Af1 = a.F1
var Af2 = a.F2
var Bt1 b.T1
var Bt2 b.T2
var Bf1 = b.F1
var Bf2 = b.F2
`,
	})

	t.Run("ObjectFacts", func(t *testing.T) {
		factAnalyzer := &analysis.Analyzer{
			Name:      "facts",
			Doc:       "exports a fact for each type and produces a diagnostic for each imported fact",
			FactTypes: []analysis.Fact{(*testFact)(nil)},
			Run: func(pass *analysis.Pass) (any, error) {
				for _, obj := range pass.TypesInfo.Defs {
					if obj == nil {
						continue
					}
					pass.ExportObjectFact(obj, &testFact{Name: obj.Name()})
				}
				for _, obj := range pass.TypesInfo.Uses {
					fact := testFact{}
					if pass.ImportObjectFact(obj, &fact) {
						pass.Reportf(pass.Files[0].Package, "fact %s", fact.Name)
					}
				}
				return nil, nil
			},
		}
		runAndCompare(t, factAnalyzer, dir, "./...")
	})

	t.Run("AllObjectFacts", func(t *testing.T) {
		factAnalyzer := &analysis.Analyzer{
			Name:      "facts",
			Doc:       "exports a fact for each type and produces a diagnostic for each imported fact",
			FactTypes: []analysis.Fact{(*testFact)(nil)},
			Run: func(pass *analysis.Pass) (any, error) {
				for _, obj := range pass.TypesInfo.Defs {
					if obj == nil {
						continue
					}
					pass.ExportObjectFact(obj, &testFact{Name: obj.Name()})
				}
				for _, fact := range pass.AllObjectFacts() {
					pass.Reportf(fact.Object.Pos(), "fact %s", fact.Fact.(*testFact).Name)
				}
				return nil, nil
			},
		}
		runAndCompare(t, factAnalyzer, dir, "./...")
	})

	t.Run("PackageFacts", func(t *testing.T) {
		factAnalyzer := &analysis.Analyzer{
			Name:      "facts",
			Doc:       "exports a fact for each package and produces a diagnostic for each imported fact",
			FactTypes: []analysis.Fact{(*testFact)(nil)},
			Run: func(pass *analysis.Pass) (any, error) {
				pass.ExportPackageFact(&testFact{Name: pass.Pkg.Name()})
				for _, pkg := range pass.Pkg.Imports() {
					fact := testFact{}
					if pass.ImportPackageFact(pkg, &fact) {
						pass.Reportf(pkg.Scope().Pos(), "fact %s", fact.Name)
					}
				}
				return nil, nil
			},
		}
		runAndCompare(t, factAnalyzer, dir, "./...")
	})

	t.Run("AllPackageFacts", func(t *testing.T) {
		factAnalyzer := &analysis.Analyzer{
			Name:      "facts",
			Doc:       "exports a fact for each package and produces a diagnostic for each imported fact",
			FactTypes: []analysis.Fact{(*testFact)(nil)},
			Run: func(pass *analysis.Pass) (any, error) {
				pass.ExportPackageFact(&testFact{Name: pass.Pkg.Name()})

				for _, fact := range pass.AllPackageFacts() {
					pass.Reportf(fact.Package.Scope().Pos(), "fact %s", fact.Fact.(*testFact).Name)
				}
				return nil, nil
			},
		}
		runAndCompare(t, factAnalyzer, dir, "./...")
	})
}

// TestFactIsolation verifies that facts exported during one benchmark
// iteration do not leak into subsequent iterations. Each call to
// [passTemplate.buildPass] must start from the same precomputed state.
func TestFactIsolation(t *testing.T) {
	dir := WriteFiles(t, map[string]string{
		"a/a.go": `package a

func Foo() {}
`,
	})

	// calls counts how many times Run has been invoked. Each invocation
	// exports facts labelled "call-N", making iterations distinguishable.
	var calls int

	factAnalyzer := &analysis.Analyzer{
		Name:      "isolation",
		Doc:       "exports facts with call-specific values to verify iteration isolation",
		FactTypes: []analysis.Fact{(*testFact)(nil)},
		Run: func(pass *analysis.Pass) (any, error) {
			calls++
			label := fmt.Sprintf("call-%d", calls)

			pass.ExportPackageFact(&testFact{Name: label})
			for _, obj := range pass.TypesInfo.Defs {
				if obj == nil {
					continue
				}
				pass.ExportObjectFact(obj, &testFact{Name: label})
			}
			return nil, nil
		},
	}

	pkgs, err := loadPackages(dir, "a")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	templates := precompute(t, factAnalyzer, pkgs)
	if len(templates) == 0 {
		t.Fatal("no templates produced")
	}
	passTempl := templates[0]

	precomputeLabel := "call-1"
	origObjectFactsLen := len(passTempl.objectFacts)
	origPackageFactsLen := len(passTempl.packageFacts)

	pkgKey := packageFactKey{passTempl.pkg.Types, reflect.TypeOf((*testFact)(nil))}

	for i := range 3 {
		pass, objFacts, pkgFacts := passTempl.buildPass(factAnalyzer, nopReport)

		// Before Run: package fact should match the precomputed label.
		if f, ok := pkgFacts[pkgKey]; !ok {
			t.Fatalf("iteration %d: precomputed package fact missing before Run", i)
		} else if got := f.(*testFact).Name; got != precomputeLabel {
			t.Errorf("iteration %d: package fact before Run = %q, want %q (isolation failure)", i, got, precomputeLabel)
		}

		// Before Run: all object facts should match the precomputed label.
		for key, f := range objFacts {
			if got := f.(*testFact).Name; got != precomputeLabel {
				t.Errorf("iteration %d: object fact for %s before Run = %q, want %q (isolation failure)",
					i, key.obj.Name(), got, precomputeLabel)
			}
		}

		// Run the analyzer and overwrite the facts with a new label.
		if _, err := factAnalyzer.Run(pass); err != nil {
			t.Fatalf("iteration %d: Run: %v", i, err)
		}

		newLabel := fmt.Sprintf("call-%d", i+2)
		if f := pkgFacts[pkgKey]; f.(*testFact).Name != newLabel {
			t.Errorf("iteration %d: package fact after Run = %q, want %q", i, f.(*testFact).Name, newLabel)
		}

		if len(passTempl.objectFacts) != origObjectFactsLen {
			t.Errorf("iteration %d: template objectFacts length changed: %d -> %d",
				i, origObjectFactsLen, len(passTempl.objectFacts))
		}
		if len(passTempl.packageFacts) != origPackageFactsLen {
			t.Errorf("iteration %d: template packageFacts length changed: %d -> %d",
				i, origPackageFactsLen, len(passTempl.packageFacts))
		}

		// Verify the template fact values themselves were not overwritten.
		if f, ok := passTempl.packageFacts[pkgKey]; !ok {
			t.Errorf("iteration %d: template package fact disappeared", i)
		} else if got := f.(*testFact).Name; got != precomputeLabel {
			t.Errorf("iteration %d: template package fact = %q, want %q (mutated)", i, got, precomputeLabel)
		}
	}
}

// funcFact is an object fact attached to function declarations.
type funcFact struct{ Label string }

func (*funcFact) AFact()           {}
func (f *funcFact) String() string { return fmt.Sprintf("funcFact:%s", f.Label) }

// pkgSummaryFact is a package fact recording the number of functions.
type pkgSummaryFact struct{ FuncCount int }

func (*pkgSummaryFact) AFact()           {}
func (f *pkgSummaryFact) String() string { return fmt.Sprintf("pkgSummaryFact:%d", f.FuncCount) }

// TestCombined exercises multiple fact types (object and package),
// a dependency analyzer with a ResultType, and cross-package fact
// import in a single runAndCompare call.
func TestCombined(t *testing.T) {
	dir := WriteFiles(t, map[string]string{
		"a/a.go": `package a

func Foo() {}
func Bar() {}
`,
		"b/b.go": `package b

import "a"

var _ = a.Foo

func Baz() {}
`,
	})

	countAnalyzer := &analysis.Analyzer{
		Name:       "count",
		Doc:        "returns the number of function declarations",
		ResultType: reflect.TypeFor[int](),
		Run: func(pass *analysis.Pass) (any, error) {
			count := 0
			for _, f := range pass.Files {
				for _, d := range f.Decls {
					if _, ok := d.(*ast.FuncDecl); ok {
						count++
					}
				}
			}
			return count, nil
		},
	}

	combinedAnalyzer := &analysis.Analyzer{
		Name:      "combined",
		Doc:       "exercises multiple fact types, requires, and cross-package fact import",
		Requires:  []*analysis.Analyzer{countAnalyzer},
		FactTypes: []analysis.Fact{(*funcFact)(nil), (*pkgSummaryFact)(nil)},
		Run: func(pass *analysis.Pass) (any, error) {
			funcCount := pass.ResultOf[countAnalyzer].(int)
			pass.ExportPackageFact(&pkgSummaryFact{FuncCount: funcCount})
			for _, f := range pass.Files {
				for _, d := range f.Decls {
					fn, ok := d.(*ast.FuncDecl)
					if !ok {
						continue
					}
					obj := pass.TypesInfo.Defs[fn.Name]
					pass.ExportObjectFact(obj, &funcFact{Label: fn.Name.Name})
				}
			}
			for _, pkg := range pass.Pkg.Imports() {
				var sf pkgSummaryFact
				if pass.ImportPackageFact(pkg, &sf) {
					pass.Reportf(pass.Files[0].Package, "pkg %s has %d funcs", pkg.Path(), sf.FuncCount)
				}
			}
			for _, obj := range pass.TypesInfo.Uses {
				var ff funcFact
				if pass.ImportObjectFact(obj, &ff) {
					pass.Reportf(pass.Files[0].Package, "uses %s", ff.Label)
				}
			}

			return nil, nil
		},
	}

	runAndCompare(t, combinedAnalyzer, dir, "./...")
}

// TestBenchmark exercises Run and RunPerPackage through
// testing.Benchmark to verify it does not panic or fatal.
func TestBenchmark(t *testing.T) {
	dir := WriteFiles(t, map[string]string{
		"a/a.go": `package a

func Foo() {}
`,
		"b/b.go": `package b

import "a"

var _ = a.Foo

func Bar() {}
`,
	})

	noopAnalyzer := &analysis.Analyzer{
		Name: "noop",
		Doc:  "does nothing",
		Run: func(pass *analysis.Pass) (any, error) {
			return nil, nil
		},
	}

	t.Run("Run", func(t *testing.T) {
		testing.Benchmark(func(b *testing.B) {
			Run(b, dir, noopAnalyzer, "./...")
		})
	})
	t.Run("RunPerPackage", func(t *testing.T) {
		testing.Benchmark(func(b *testing.B) {
			RunPerPackage(b, dir, noopAnalyzer, "./...")
		})
	})
}

// TestError verifies that Run and RunPerPackage do not error even if the analyzer errors.
// Error checking is the responsibility of the analysistest package.
func TestError(t *testing.T) {
	dir := WriteFiles(t, map[string]string{
		"a/a.go": `package a`,
	})
	errorAnalyzer := &analysis.Analyzer{
		Name: "error",
		Doc:  "returns an error",
		Run: func(pass *analysis.Pass) (any, error) {
			return nil, errors.New("analysis error")
		},
	}
	testing.Benchmark(func(b *testing.B) {
		Run(b, dir, errorAnalyzer, "./...")
	})
	testing.Benchmark(func(b *testing.B) {
		RunPerPackage(b, dir, errorAnalyzer, "./...")
	})
}

func BenchmarkNoop(b *testing.B) {
	// import heavy packages to generate decent workload
	dir := WriteFiles(b, map[string]string{
		"a/heavy.go": `package heavytest

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"unsafe"

	_ "net/http/pprof"
)

// Deep Type Recursion
type DeepNest struct {
	Next *DeepNest
	Client http.Client
	Server httptest.Server
}

// Reflection & Unsafe
func ReflectionLoad() {
	var x int = 42
	v := reflect.ValueOf(&x).Elem()
	p := unsafe.Pointer(v.UnsafeAddr())
	_ = p
}

// Standard Library Soup
func ProtocolLoad() {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := ts.Client()
	_, _ = client.Get(ts.URL)
}

// Heavy Generics
type Graph[T any] struct {
	Nodes map[string]T
}

func (g *Graph[T]) Link(a, b string) {}
`,
	})
	noopAnalyzer := &analysis.Analyzer{
		Name: "noop",
		Doc:  "does nothing",
		Run: func(pass *analysis.Pass) (any, error) {
			return nil, nil
		},
	}
	noopWithInspectAnalyzer := &analysis.Analyzer{
		Name: "noop-with-requires",
		Doc:  "does nothing",
		Requires: []*analysis.Analyzer{
			inspect.Analyzer,
		},
		Run: func(pass *analysis.Pass) (any, error) {
			return nil, nil
		},
	}
	b.Run("Noop", func(b *testing.B) {
		RunPerPackage(b, dir, noopAnalyzer, "./...")
	})
	b.Run("NoopWithInspect", func(b *testing.B) {
		RunPerPackage(b, dir, noopWithInspectAnalyzer, "./...")
	})
}
