# contrib todos — small, test-gated tasks for the self-hosting loop

Each todo is a small, objective contribution to `src/` that a failing test pins
down. The loop (`tools/contrib-loop.mjs`) runs each in an isolated sandbox: qwen
on the box reads the code **through file-projections' own lens** (self-projection),
implements the change, and the change is accepted **only if `go test ./src` goes
green**. Green changes are copied back into the main repo ("that's good").

Format per todo: a fenced `todo` block the loop parses.

```todo
id: clamp-int
title: add clampInt helper
file: src/util.go
test_name: TestClampInt
view: util.go atoiDefault
test: |
  func TestClampInt(t *testing.T) {
      cases := []struct{ n, lo, hi, want int }{
          {5, 0, 10, 5}, {-3, 0, 10, 0}, {99, 0, 10, 10}, {7, 7, 7, 7},
      }
      for _, c := range cases {
          if got := clampInt(c.n, c.lo, c.hi); got != c.want {
              t.Fatalf("clampInt(%d,%d,%d)=%d want %d", c.n, c.lo, c.hi, got, c.want)
          }
      }
  }
instruction: |
  Add an exported-style helper `func clampInt(n, lo, hi int) int` to src/util.go
  that returns n constrained to the inclusive range [lo, hi]. Match the style of
  the nearby tiny helpers (atoi, atoiDefault). Do not change any other code.
```

```todo
id: fact-by-id
title: add Projection.FactByID lookup
file: src/projection.go
test_name: TestFactByID
view: registry.go ExecuteLens
test: |
  func TestFactByID(t *testing.T) {
      p := Projection{Facts: []ProjectionFact{{ID: "a", Text: "x"}, {ID: "b", Text: "y"}}}
      f, ok := p.FactByID("b")
      if !ok || f.Text != "y" {
          t.Fatalf("FactByID(b) = %+v, %v", f, ok)
      }
      if _, ok := p.FactByID("z"); ok {
          t.Fatalf("FactByID(z) should be false")
      }
  }
instruction: |
  Add a method `func (p Projection) FactByID(id string) (ProjectionFact, bool)`
  to src/projection.go that returns the fact whose ID equals id (and true), or the
  zero ProjectionFact and false. Use the Projection and ProjectionFact types as-is.
```

```todo
id: lens-by-name
title: add Config.LensByName lookup
file: src/config.go
test_name: TestLensByName
view: config.go LoadConfig
test: |
  func TestLensByName(t *testing.T) {
      cfg := Config{Lenses: []LensConfig{{Name: "a"}, {Name: "b", Analyzer: "go-symbols"}}}
      l, ok := cfg.LensByName("b")
      if !ok || l.Analyzer != "go-symbols" {
          t.Fatalf("LensByName(b) = %+v, %v", l, ok)
      }
      if _, ok := cfg.LensByName("missing"); ok {
          t.Fatalf("LensByName(missing) should be false")
      }
  }
instruction: |
  Add a method `func (c Config) LensByName(name string) (LensConfig, bool)` to
  src/config.go that returns the lens with the given Name (and true), or the zero
  LensConfig and false if none matches. Use the Config and LensConfig types as-is.
```
