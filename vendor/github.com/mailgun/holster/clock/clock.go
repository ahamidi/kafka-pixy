// Package clock provides the same functions as the system package time. In
// production it forwards all calls to the system time package, but in tests
// the time can be frozen by calling Freeze function and from that point it has
// to be advanced manually with Advance function making all scheduled calls
// deterministic.
//
// The functions provided by the package have the same parameters and return
// values as their system counterparts with a few exceptions. Where either
// *time.Timer or *time.Ticker is returned by a system function, the clock
// package counterpart returns clock.Timer or clock.Ticker interface
// respectively. The interfaces provide API as respective structs except C is
// not a channel, but a function that returns <-chan time.Time.
package clock

import "time"

var frozenAt time.Time

// Freeze after this function is called all time related functions start
// generate deterministic timers that are triggered by Advance function. It is
// supposed to be used in tests only.
func Freeze(now time.Time) {
	frozenAt = now.UTC()
	provider = &frozenTime{now: now}
}

// Unfreeze reverses effect of Freeze.
func Unfreeze() {
	provider = &systemTime{}
}

// Makes the deterministic time move forward by the specified duration, firing
// timers along the way in the natural order. It returns how much time has
// passed since it was frozen. So you can assert on the return value in tests
// to make it explicit where you stand on the deterministic time scale.
func Advance(d time.Duration) time.Duration {
	ft, ok := provider.(*frozenTime)
	if !ok {
		panic("Freeze time first!")
	}
	ft.advance(d)
	return Now().UTC().Sub(frozenAt)
}

// Now see time.Now.
func Now() time.Time {
	return provider.Now()
}

// Sleep see time.Sleep.
func Sleep(d time.Duration) {
	provider.Sleep(d)
}

// After see time.After.
func After(d time.Duration) <-chan time.Time {
	return provider.After(d)
}

// Timer see time.Timer.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// NewTimer see time.NewTimer.
func NewTimer(d time.Duration) Timer {
	return provider.NewTimer(d)
}

// AfterFunc see time.AfterFunc.
func AfterFunc(d time.Duration, f func()) Timer {
	return provider.AfterFunc(d, f)
}

// Ticker see time.Ticker.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// NewTicker see time.Ticker.
func NewTicker(d time.Duration) Ticker {
	return provider.NewTicker(d)
}

// Tick see time.Tick.
func Tick(d time.Duration) <-chan time.Time {
	return provider.Tick(d)
}

// NewStoppedTimer returns a stopped timer. Call Reset to get it ticking.
func NewStoppedTimer() Timer {
	t := NewTimer(42 * time.Hour)
	t.Stop()
	return t
}

type clock interface {
	Now() time.Time
	Sleep(d time.Duration)
	After(d time.Duration) <-chan time.Time
	NewTimer(d time.Duration) Timer
	AfterFunc(d time.Duration, f func()) Timer
	NewTicker(d time.Duration) Ticker
	Tick(d time.Duration) <-chan time.Time
}

var provider clock = &systemTime{}
