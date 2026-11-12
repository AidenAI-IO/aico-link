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
