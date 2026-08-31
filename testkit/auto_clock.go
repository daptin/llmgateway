package testkit

import (
	"sync"
	"time"

	"github.com/daptin/llmgateway"
)

// AutoClock advances immediately when a timer is created. It is useful for
// deterministic retry tests that do not need to observe the waiting state.
type AutoClock struct {
	mu  sync.Mutex
	now time.Time
}

func NewAutoClock(now time.Time) *AutoClock { return &AutoClock{now: now} }

func (c *AutoClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *AutoClock) NewTimer(duration time.Duration) llmgateway.Timer {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	now := c.now
	c.mu.Unlock()
	channel := make(chan time.Time, 1)
	channel <- now
	return autoTimer{channel: channel}
}

type autoTimer struct{ channel <-chan time.Time }

func (t autoTimer) C() <-chan time.Time { return t.channel }
func (autoTimer) Stop() bool            { return false }
