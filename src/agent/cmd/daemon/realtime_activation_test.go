package main

import (
	"testing"

	"aico-agent/internal/agent"
)

// A device activation that arrives at any point while a finished session is
// settled must survive: it stays recorded and wakes the loop again.
func TestRealtimeActivationsKeepDeviceWakeDuringSettle(t *testing.T) {
	for _, stage := range []string{"cleared", "drained"} {
		t.Run(stage, func(t *testing.T) {
			a := newRealtimeActivations()
			a.Device() // Served by the session that just ended.
			<-a.events
			a.stage = func(at string) {
				if at == stage {
					a.Device()
				}
			}
			a.Settle(false, func() bool { return false })
			select {
			case <-a.events:
			default:
				t.Fatal("a device activation during settle did not wake the loop")
			}
			if !a.TakeDevice() {
				t.Fatal("a device activation during settle was forgotten; the loop would open no session")
			}
		})
	}
}

// An activation the session served is cleared, so a later browser-only
// wakeup does not open the device.
func TestRealtimeActivationsClearServedDeviceWake(t *testing.T) {
	a := newRealtimeActivations()
	a.Device()
	a.Settle(false, func() bool { return false })
	select {
	case <-a.events:
		t.Fatal("a served activation woke the loop again")
	default:
	}
	if a.TakeDevice() {
		t.Fatal("a served activation still claims the device")
	}
}

// A task update deferred behind a failure announcement must still open the
// device when the loop wakes itself after the announcement.
func TestRealtimeActivationsKeepDeferredDeviceWake(t *testing.T) {
	a := newRealtimeActivations()
	a.DeferDevice()
	// The loop re-signals after the announcement without naming a source.
	a.Wake()
	<-a.events
	if !a.TakeDevice() {
		t.Fatal("a deferred task update did not claim the device")
	}
}

type fakeRealtimeClaimer struct{ session *agent.RealtimeVoiceSession }

func (f fakeRealtimeClaimer) ClaimRealtimeVoiceSession(agent.Config, bool) *agent.RealtimeVoiceSession {
	return f.session
}

// A wakeup that opens no session, such as a withdrawn browser start, must not
// advance the task watermark; otherwise a task update queued behind it would
// look already announced and never open a session.
func TestWithdrawnWakeupKeepsTaskUpdateActivatable(t *testing.T) {
	var taskWake taskWakeState
	const pendingUpdate = 7
	if _, opened := claimRealtimeSessionMedia(fakeRealtimeClaimer{}, agent.Config{}, newRealtimeActivations(), &taskWake, pendingUpdate, false); opened {
		t.Fatal("a withdrawn browser start opened a session")
	}
	if !taskWake.shouldActivate(pendingUpdate) {
		t.Fatal("the task update queued behind a withdrawn wakeup can no longer activate the loop")
	}
}

// A wakeup that does open a session records the updates it will consume.
func TestOpenedSessionConsumesTaskWatermark(t *testing.T) {
	var taskWake taskWakeState
	const pendingUpdate = 7
	session := &agent.RealtimeVoiceSession{}
	if _, opened := claimRealtimeSessionMedia(fakeRealtimeClaimer{session: session}, agent.Config{}, newRealtimeActivations(), &taskWake, pendingUpdate, true); !opened {
		t.Fatal("a device activation opened no session")
	}
	if taskWake.shouldActivate(pendingUpdate) {
		t.Fatal("an update the session consumed would activate the loop again")
	}
}

// A board wakeup while a session runs is ignored; in standby it opens the
// device.
func TestRealtimeActivationsIgnoreWakeupWhileRunning(t *testing.T) {
	a := newRealtimeActivations()
	a.SetRunning(true)
	a.Wakeup()
	select {
	case <-a.events:
		t.Fatal("a wakeup during a session woke the loop")
	default:
	}
	if a.TakeDevice() {
		t.Fatal("a wakeup during a session claimed the device")
	}
	a.SetRunning(false)
	a.Wakeup()
	<-a.events
	if !a.TakeDevice() {
		t.Fatal("a wakeup in standby did not claim the device")
	}
}
