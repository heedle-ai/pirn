package pirn

import (
	"context"
	"time"

	"go.temporal.io/sdk/activity"
)

// SetStepBeatEvery sets how often agent steps heartbeat, and how long a
// quiet one keeps on, and calls beat at each of those heartbeats (as
// well as sending it), until the returned func is called. Temporal's
// SDK throttles the heartbeats it sends, so a test can't count them.
func SetStepBeatEvery(every, quiet time.Duration, beat func()) func() {
	e, q := stepBeatEvery, beatQuiet
	stepBeatEvery, beatQuiet = every, quiet
	recordHeartbeat = func(ctx context.Context, details ...any) {
		beat()
		activity.RecordHeartbeat(ctx, details...)
	}
	return func() { stepBeatEvery, beatQuiet, recordHeartbeat = e, q, activity.RecordHeartbeat }
}

// ReadsOnly are the tools a payment page allows besides leaving it.
var ReadsOnly = readsOnly
