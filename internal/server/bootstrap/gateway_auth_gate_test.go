package bootstrap

import "testing"

func TestAuthGateRejectsAtCapacityAndReusesReleasedSlot(t *testing.T) {
	gate := newAuthGate(1)
	firstRelease, acquired := gate.tryAcquire()
	if !acquired {
		t.Fatal("first auth slot was not acquired")
	}
	if _, acquired := gate.tryAcquire(); acquired {
		t.Fatal("auth gate accepted a connection above capacity")
	}

	firstRelease()
	secondRelease, acquired := gate.tryAcquire()
	if !acquired {
		t.Fatal("released auth slot was not reusable")
	}
	secondRelease()
	if got := len(gate.slots); got != 0 {
		t.Fatalf("auth slots in use = %d, want 0", got)
	}
}

func TestAuthGateWithInvalidCapacityFailsClosed(t *testing.T) {
	gate := newAuthGate(0)
	if _, acquired := gate.tryAcquire(); acquired {
		t.Fatal("zero-capacity auth gate unexpectedly accepted a connection")
	}
}
