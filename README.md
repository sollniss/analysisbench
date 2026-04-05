# analysisbench

analysisbench provides utilities for benchmarking Go analyzers.

It mirrors the ergonomics of [analysistest](https://pkg.go.dev/golang.org/x/tools/go/analysis/analysistest), but targets `testing.B` instead of `testing.T`.

Unlike a naive approach that calls [checker.Analyze](https://pkg.go.dev/golang.org/x/tools/go/analysis/checker#Analyze) in the benchmark loop (which would re-run dependency analyzers such as `inspect.Analyzer` on every iteration), this package pre-computes the results of all required analyzers once and then benchmarks only the target analyzer's Run function.

The hot loop calls `Analyzer.Run` directly, bypassing `checker.Analyze` entirely.

Analyzers that produce or consume facts are fully supported: the complete analysis is executed once before the benchmark loop, and the resulting facts are injected into each benchmarked pass. Fact maps are cloned per iteration so that exports made during a run are immediately visible to imports and `AllFacts` calls within the same iteration (coherent read/write), but do not persist across iterations.

Diagnostics result in a no-op and should have minimal influence the benchmark result.

A typical benchmark looks like:

```go
func BenchmarkMyAnalyzer(b *testing.B) {
    testdata := analysisbench.TestData()
    analysisbench.Run(b, testdata, myAnalyzer, "mypackage/...")
}
```

For per-package granularity, use [RunPerPackage] instead:

```go
func BenchmarkMyAnalyzer(b *testing.B) {
    testdata := analysisbench.TestData()
    analysisbench.RunPerPackage(b, testdata, myAnalyzer, "mypackage/...")
}
```

# Framework overhead

Benchmarking a noop analyzer will result in the following baseline results (calculated with `BenchmarkNoop`):

```
400-480 ns/op 456 B/op 12 allocs/op
```

Test System:

```
goos: linux
goarch: amd64
cpu: AMD Ryzen 5 3600 6-Core Processor
```

Values may increase with more exported facts and/or dependencies.
This cost is unavoidable because the framework needs to create an `analysis.Pass` for each benchmark run.
