package main

import "testing"

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
			a.Settle(true, false, func() bool { return false })
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
	a.Settle(true, false, func() bool { return false })
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

// A wake word during a text session ends it and is kept for the next
// session, which then opens the device; a text session serves no device
// activation.
func TestRealtimeActivationsWakeupEndsTextSession(t *testing.T) {
	a := newRealtimeActivations()
	stopped := 0
	a.SetTextSession(func() { stopped++ })
	a.Wakeup()
	if stopped != 1 {
		t.Fatalf("text session stopped %d times, want 1", stopped)
	}
	a.SetTextSession(nil)
	a.Settle(false, false, func() bool { return false })
	<-a.events
	if !a.TakeDevice() {
		t.Fatal("a wakeup during a text session did not claim the device")
	}

	// One recorded while the loop was claiming the text session ends it as
	// soon as it is published.
	a.DeferDevice()
	a.SetTextSession(func() { stopped++ })
	if stopped != 2 {
		t.Fatal("a device activation recorded before the text session started did not end it")
	}
}
