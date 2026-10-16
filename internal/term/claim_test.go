package term

import (
	"testing"
	"time"
)

// A tab that /clears moves to a second conversation. Both are its own, and both
// become free the moment its process is gone — the abandoned one no less than
// the current one.
func TestClaimHistoryOutlivesTheSession(t *testing.T) {
	m := NewManager()
	s, err := m.Start("t1", KindAgent, "claude", ".", []string{"cat"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Claim("first")
	s.Claim("second")

	if got := m.ClaimedIDs("t1"); !got["second"] || got["first"] {
		t.Errorf("live tab claims %v, want only the current conversation", got)
	}
	if got := m.ReleasedIDs("t1"); len(got) != 0 {
		t.Errorf("nothing has exited yet, but %v is reported free", got)
	}

	m.Close(s.ID)

	if got := m.ClaimedIDs("t1"); len(got) != 0 {
		t.Errorf("a closed tab still claims %v", got)
	}
	released := m.ReleasedIDs("t1")
	if !released["first"] || !released["second"] {
		t.Errorf("got %v, want both conversations free", released)
	}
}

// A tab whose agent exited on its own is still on screen, showing "[process
// exited]". Its conversation has no writer and reopening it is the only thing
// left to do, so the claim must not survive the process.
func TestDeadTabReleasesItsConversation(t *testing.T) {
	m := NewManager()
	s, err := m.Start("t2", KindAgent, "claude", ".", []string{"true"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(s.ID)
	s.Claim("conv")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && s.Alive() {
		time.Sleep(20 * time.Millisecond)
	}
	if s.Alive() {
		t.Fatal("the process never exited")
	}
	if got := m.ClaimedIDs("t2"); got["conv"] {
		t.Error("a dead tab must not keep its conversation claimed")
	}
	if got := m.ReleasedIDs("t2"); !got["conv"] {
		t.Errorf("got %v, want conv reported free", got)
	}
}

// Shell tabs have no conversation, and a session that never claimed one must
// not put an empty id into either set.
func TestShellSessionsClaimNothing(t *testing.T) {
	m := NewManager()
	s, err := m.Start("t3", KindShell, "shell", ".", []string{"cat"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Close(s.ID)
	if got := m.ReleasedIDs("t3"); len(got) != 0 {
		t.Errorf("got %v, want nothing", got)
	}
}
