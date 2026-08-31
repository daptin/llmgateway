package testkit

import (
	"testing"
	"time"
)

func TestClockFiresAtDeterministicDeadline(t *testing.T) {
	start := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	clock := NewClock(start)
	timer := clock.NewTimer(time.Second)
	clock.Advance(999 * time.Millisecond)
	select {
	case <-timer.C():
		t.Fatal("timer fired early")
	default:
	}
	clock.Advance(time.Millisecond)
	select {
	case fired := <-timer.C():
		if !fired.Equal(start.Add(time.Second)) {
			t.Fatalf("fired=%s", fired)
		}
	default:
		t.Fatal("timer did not fire")
	}
}

func TestStoppedTimerDoesNotFire(t *testing.T) {
	clock := NewClock(time.Time{})
	timer := clock.NewTimer(time.Second)
	if !timer.Stop() {
		t.Fatal("expected active timer to stop")
	}
	clock.Advance(time.Second)
	select {
	case <-timer.C():
		t.Fatal("stopped timer fired")
	default:
	}
}
