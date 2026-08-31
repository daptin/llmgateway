package llmgateway

import (
	"math/rand/v2"
	"time"
)

// SystemClock is the production wall clock implementation.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) NewTimer(duration time.Duration) Timer {
	return systemTimer{Timer: time.NewTimer(duration)}
}

type systemTimer struct{ *time.Timer }

func (t systemTimer) C() <-chan time.Time { return t.Timer.C }

// RandomSelector is the production weighted-route selector. Tests should use
// an injected deterministic selector instead.
type RandomSelector struct{}

func (RandomSelector) Intn(limit int) int { return rand.IntN(limit) }
