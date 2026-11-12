package main

import (
	"sync/atomic"

	"aico-agent/internal/logging"
)

// realtimeActivations tracks why the realtime wakeup loop was woken.
//
// Every source shares one coalescing wakeup channel. Device sources (wake
// word, MCU, chat, task updates) also record that they want the device, so
// the loop can tell them apart from a browser voice start, which must never
// fall back to the board microphone once it is withdrawn.
type realtimeActivations struct {
	events chan struct{}
	device atomic.Bool
	// running is set while a conversation runs. A wake word or button press
	// then belongs to that conversation and is dropped.
	running atomic.Bool
	// stage, when set by tests, runs at each step of settle so a test can
	// interleave an activation at a precise point.
	stage func(string)
}

func newRealtimeActivations() *realtimeActivations {
	return &realtimeActivations{events: make(chan struct{}, 1)}
}

// Device records a device activation and wakes the loop.
func (a *realtimeActivations) Device() {
	a.device.Store(true)
	signalWakeupEvent(a.events)
}

// Wakeup handles a wake word or MCU button press. It is ignored while a
// session runs: the user is already talking. Dropping it here, instead of
// leaving it for Settle to clear, also keeps it from cutting short the
// failure announcement of a session that goes on to fail.
func (a *realtimeActivations) Wakeup() {
	if a.running.Load() {
		logging.Infof("agent", "wakeup", "Wakeup ignored: a realtime session is already running")
		return
	}
	a.Device()
}

// SetRunning marks whether a session is running.
func (a *realtimeActivations) SetRunning(running bool) {
	a.running.Store(running)
}

// DeferDevice records a device activation without waking the loop, for a
// loop that will wake itself later, such as after a failure announcement.
func (a *realtimeActivations) DeferDevice() {
	a.device.Store(true)
}

// Wake wakes the loop without claiming the device, for a browser voice start
// or a re-signal of an activation that is already recorded.
func (a *realtimeActivations) Wake() {
	signalWakeupEvent(a.events)
}

// TakeDevice reports and clears a recorded device activation.
func (a *realtimeActivations) TakeDevice() bool {
	return a.device.Swap(false)
}

// Settle runs after a session ends. Activations served by that session are
// cleared and their queued wakeups drained; one that arrives at any point
// during Settle is kept and wakes the loop again. keep wakes the loop for
// another reason, such as a provider rotation or a queued chat request.
func (a *realtimeActivations) Settle(failureAnnouncementPending bool, keep func() bool) {
	a.device.Store(false)
	a.at("cleared")
	drainRealtimeWakeups(a.events, failureAnnouncementPending)
	a.at("drained")
	if a.device.Load() || keep() {
		a.Wake()
	}
}

func (a *realtimeActivations) at(stage string) {
	if a.stage != nil {
		a.stage(stage)
	}
}
