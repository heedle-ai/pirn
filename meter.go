package pirn

import (
	"context"
	"sync"
)

// Counting every call (roadmap 10.4.1). A step's output records what its
// requests used, but only when the step succeeds and its run is recorded.
// A Meter in the context sees every request Claude answers, whatever then
// becomes of the step, so loom can count the calls no record holds: an
// attempt that failed, a run that failed, the front door's own asks.

// Call is one request Claude answered: the model that answered and the
// tokens it counted.
type Call struct {
	Model  string `json:"model"`
	Input  int64  `json:"input"`
	Output int64  `json:"output"`
}

// Meter collects the Calls made under a context (WithMeter). It is safe
// for concurrent use, and a nil Meter counts nothing.
type Meter struct {
	mu    sync.Mutex
	calls []Call
}

// Add notes one call. pirn adds every request Claude answers, the moment
// it answers, before anything else can fail.
func (m *Meter) Add(c Call) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.calls = append(m.calls, c)
	m.mu.Unlock()
}

// Calls are the calls noted so far, in order.
func (m *Meter) Calls() []Call {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Call(nil), m.calls...)
}

type meterKey struct{}

// WithMeter is ctx with m counting every request Claude answers under it.
func WithMeter(ctx context.Context, m *Meter) context.Context {
	return context.WithValue(ctx, meterKey{}, m)
}

// MeterFrom is the Meter ctx carries, or nil.
func MeterFrom(ctx context.Context) *Meter {
	m, _ := ctx.Value(meterKey{}).(*Meter)
	return m
}
