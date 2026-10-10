package party

import "testing"

// A removed host must not keep host powers. Its socket can outlive its
// membership, and MayControl and SetMaxMembers both read IsHost from the Member.
func TestRemovedHostLosesHostPowers(t *testing.T) {
	p := NewParty("REMHST")
	host, _ := p.Join("u1", "d1", "Host", nil)
	listener, _ := p.Join("u2", "d2", "Listener", nil)
	if err := p.SetHostOnlyControl(host, true); err != nil {
		t.Fatalf("host was refused: %v", err)
	}

	p.Remove(host.MemberId)

	if host.IsHost {
		t.Fatalf("removed host still reports IsHost")
	}
	if p.MayControl(host) {
		t.Errorf("removed host may still control playback under host-only control")
	}
	if err := p.SetMaxMembers(host, 3); err == nil {
		t.Errorf("removed host may still resize the party")
	}
	if !listener.IsHost {
		t.Errorf("the remaining member was not promoted")
	}
}
