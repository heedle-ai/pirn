package pirn

import (
	"context"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
)

// Agent steps heartbeat to Temporal every stepBeatEvery while they run,
// not only after each command (roadmap 9.2, review 50): Temporal tells an
// activity it was cancelled only through a heartbeat, so a step waiting
// on Claude heard of a cancel only after its next command (PIRN-502). A
// step with no progress for beatQuiet stops beating, so Temporal still
// notices a hung step, by AgentOptions.Heartbeat after that.
var (
	stepBeatEvery   = 10 * time.Second
	beatQuiet       = AgentOptions.Heartbeat - time.Minute
	recordHeartbeat = activity.RecordHeartbeat
)

// heartbeating makes a heartbeat to Temporal every stepBeatEvery, with the
// number of commands run so far, until stop is called or a step has made
// no progress for beatQuiet. It wraps a.OnProgress to see progress.
func (a *Agent) heartbeating(ctx context.Context) (stop func()) {
	var mu sync.Mutex
	last, commands := time.Now(), 0
	progress := a.OnProgress
	a.OnProgress = func(r AgentResult) {
		mu.Lock()
		last, commands = time.Now(), len(r.Commands)
		mu.Unlock()
		if progress != nil {
			progress(r)
		}
	}
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(stepBeatEvery)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				mu.Lock()
				quiet, n := time.Since(last), commands
				mu.Unlock()
				if quiet < beatQuiet {
					recordHeartbeat(ctx, n)
				}
			}
		}
	}()
	return func() { close(quit); <-done }
}
