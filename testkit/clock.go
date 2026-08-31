package testkit

import (
	"sync"
	"time"

	"github.com/daptin/llmgateway"
)

type Clock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*timer]struct{}
}

type timer struct {
	clock   *Clock
	when    time.Time
	channel chan time.Time
	active  bool
}

func NewClock(now time.Time) *Clock {
	return &Clock{now: now, timers: make(map[*timer]struct{})}
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) NewTimer(duration time.Duration) llmgateway.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	value := &timer{clock: c, when: c.now.Add(duration), channel: make(chan time.Time, 1), active: true}
	c.timers[value] = struct{}{}
	return value
}

func (c *Clock) Advance(duration time.Duration) {
	if duration < 0 {
		panic("testkit.Clock cannot move backwards")
	}
	c.mu.Lock()
	c.now = c.now.Add(duration)
	for value := range c.timers {
		if value.active && !value.when.After(c.now) {
			value.active = false
			delete(c.timers, value)
			value.channel <- value.when
		}
	}
	c.mu.Unlock()
}

func (t *timer) C() <-chan time.Time { return t.channel }

func (t *timer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if !t.active {
		return false
	}
	t.active = false
	delete(t.clock.timers, t)
	return true
}
