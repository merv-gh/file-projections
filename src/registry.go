package main

import (
	"errors"
	"fmt"
)

// Analyzer registry and the core pipeline: Run -> ExecuteLens -> Analyzer ->
// RenderProjection. This is the language-agnostic spine; every language frontend
// is just an Analyzer registered in DefaultRegistry.

// Run executes every lens in the config and writes its projection (plus any extras).
func Run(cfg Config, registry Registry) ([]Projection, error) {
	if len(cfg.Lenses) == 0 {
		return nil, errors.New("config has no lenses")
	}
	var results []Projection
	for _, lens := range cfg.Lenses {
		p, err := ExecuteLens(cfg, registry, lens)
		if err != nil {
			return nil, fmt.Errorf("lens %s: %w", lens.Name, err)
		}
		if err := RenderProjection(LensOut(cfg, lens), p); err != nil {
			return nil, err
		}
		for _, ex := range p.Extra {
			if err := RenderProjection(ex.Path, ex.Proj); err != nil {
				return nil, err
			}
		}
		results = append(results, p)
	}
	return results, nil
}

type AnalyzerFunc struct {
	name string
	fn   func(Config, LensConfig) (Projection, error)
}

func (a AnalyzerFunc) Name() string { return a.name }

func (a AnalyzerFunc) Analyze(cfg Config, lens LensConfig) (Projection, error) {
	return a.fn(cfg, lens)
}

type Registry map[string]Analyzer

func DefaultRegistry() Registry {
	return Registry{
		"jsonl":             AnalyzerFunc{"jsonl", AnalyzeJSONL},
		"go-symbols":        AnalyzerFunc{"go-symbols", AnalyzeGoSymbols},
		"flow":              AnalyzerFunc{"flow", AnalyzeFlow},
		"java-post-to-save": AnalyzerFunc{"java-post-to-save", AnalyzeFlow}, // back-compat alias for flow
		"js-events":         AnalyzerFunc{"js-events", AnalyzeJSEvents},
		"joern-var-flow":    AnalyzerFunc{"joern-var-flow", AnalyzeJoernVarFlow},
		"entrypoints":       AnalyzerFunc{"entrypoints", AnalyzeEntrypoints},
		"exitpoints":        AnalyzerFunc{"exitpoints", AnalyzeExitpoints},
		"side-effects":      AnalyzerFunc{"side-effects", AnalyzeSideEffects},
		"ast-grep":          AnalyzerFunc{"ast-grep", AnalyzeAstGrep},
		"control-flow":      AnalyzerFunc{"control-flow", AnalyzeControlFlow},
		"entry-to-exit":     AnalyzerFunc{"entry-to-exit", AnalyzeEntryToExit},
		"data-flow":         AnalyzerFunc{"data-flow", AnalyzeDataFlow},
		"object-flow":       AnalyzerFunc{"object-flow", AnalyzeObjectFlow},
		"cpg-methods":       AnalyzerFunc{"cpg-methods", AnalyzeCPGMethods},
		"unrolled-program":  AnalyzerFunc{"unrolled-program", AnalyzeUnrolledProgram},
		"bookmark":          AnalyzerFunc{"bookmark", AnalyzeBookmark},
		"extract":           AnalyzerFunc{"extract", AnalyzeBookmark}, // back-compat alias
		"service-graph":     AnalyzerFunc{"service-graph", AnalyzeServiceGraph},
		"postgres-watch":    AnalyzerFunc{"postgres-watch", AnalyzePostgresWatch},
	}
}

func ExecuteLens(cfg Config, registry Registry, lens LensConfig) (Projection, error) {
	analyzer, ok := registry[lens.Analyzer]
	if !ok {
		return Projection{}, fmt.Errorf("unknown analyzer %q", lens.Analyzer)
	}
	p, err := analyzer.Analyze(cfg, lens)
	if err != nil {
		return Projection{}, err
	}
	p.Lens = lens
	finalizeProjection(&p, lens)
	return p, nil
}
