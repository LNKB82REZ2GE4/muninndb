package embed

import (
	"context"
	"errors"
	"testing"

	"github.com/scrypster/muninndb/internal/plugin"
)

// switchableProvider returns a configurable outcome per call.
type switchableProvider struct {
	err  error
	zero bool
}

func (p *switchableProvider) Name() string { return "switchable" }
func (p *switchableProvider) Init(_ context.Context, _ ProviderHTTPConfig) (int, error) {
	return 2, nil
}
func (p *switchableProvider) EmbedBatch(_ context.Context, texts []string) ([]float32, error) {
	if p.err != nil {
		return nil, p.err
	}
	out := make([]float32, 2*len(texts))
	if !p.zero {
		for i := range out {
			out[i] = 0.7
		}
	}
	return out, nil
}
func (p *switchableProvider) MaxBatchSize() int { return 8 }
func (p *switchableProvider) Close() error      { return nil }

// TestEmbedService_RecordsHealth pins G4's instrumentation point: every
// EmbedService.Embed outcome lands on the attached tracker — an error and a
// success-with-all-zero-vector both count as degraded, a real vector as ok.
func TestEmbedService_RecordsHealth(t *testing.T) {
	prov := &switchableProvider{}
	svc := &EmbedService{provider: prov, dim: 2}
	svc.batcher = NewBatchEmbedder(prov, nil, &svc.stats)
	h := plugin.NewEmbedHealth("embed-switchable")
	svc.SetHealth(h)
	ctx := context.Background()

	if _, err := svc.Embed(ctx, []string{"coolant pump"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := h.Snapshot().State; got != plugin.EmbedHealthOK {
		t.Fatalf("after a real vector: state %q, want ok", got)
	}

	prov.err = errors.New("401 Missing Bearer token")
	_, _ = svc.Embed(ctx, []string{"coolant pump"})
	if s := h.Snapshot(); s.State != plugin.EmbedHealthDegraded || s.ConsecutiveFailures != 1 {
		t.Fatalf("after an error: %+v, want degraded with 1 failure", s)
	}

	prov.err = nil
	prov.zero = true
	if _, err := svc.Embed(ctx, []string{"coolant pump"}); err != nil {
		t.Fatalf("Embed (zero): %v", err)
	}
	if s := h.Snapshot(); s.State != plugin.EmbedHealthDegraded || s.ConsecutiveFailures != 2 {
		t.Fatalf("after an all-zero vector: %+v, want still degraded with 2 failures", s)
	}

	prov.zero = false
	_, _ = svc.Embed(ctx, []string{"coolant pump"})
	if got := h.Snapshot().State; got != plugin.EmbedHealthOK {
		t.Fatalf("after recovery: state %q, want ok", got)
	}
}
