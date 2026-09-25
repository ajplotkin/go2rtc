package webrtc

import (
	"testing"

	"github.com/pion/webrtc/v4"
)

// The read-error guard in OnTrack warns only when
// pc.SignalingState() != webrtc.SignalingStateClosed. That rests on one claim:
// a live connection never reads Closed, and a closed one always does. If this
// fails, the guard either spams warnings on every teardown or hides the zombie.
func TestTeardownGuardStateTransition(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}

	// live: the guard must treat a read failure here as the zombie and WARN
	if got := pc.SignalingState(); got == webrtc.SignalingStateClosed {
		t.Fatalf("live connection reads %v; guard would hide a real zombie", got)
	}
	if !warnFor(pc) {
		t.Fatal("guard would stay quiet on a live connection")
	}

	if err := pc.Close(); err != nil {
		t.Fatal(err)
	}

	// torn down: any read error now is expected teardown, the guard must be QUIET
	if got := pc.SignalingState(); got != webrtc.SignalingStateClosed {
		t.Fatalf("closed connection reads %v, not Closed; guard would warn on teardown", got)
	}
	if warnFor(pc) {
		t.Fatal("guard would warn on an ordinary teardown")
	}
}

// warnFor mirrors the exact predicate used in conn.go's read-error path.
func warnFor(pc *webrtc.PeerConnection) bool {
	return pc.SignalingState() != webrtc.SignalingStateClosed
}
