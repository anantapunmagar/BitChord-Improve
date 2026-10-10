package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/KabirSinghBhatia/BitChord/backend/config"
	"github.com/KabirSinghBhatia/BitChord/backend/party"
	"github.com/KabirSinghBhatia/BitChord/backend/protocol"
)

// newTestParty creates a party directly in the store. POST /api/parties is rate
// limited per IP, and every test server shares 127.0.0.1.
func newTestParty(t *testing.T) (*party.Party, *party.Member) {
	t.Helper()
	p, err := store.CreateWithLimits(config.MaxParties, config.MaxMembers)
	if err != nil {
		t.Fatalf("create party: %v", err)
	}
	p.Lock()
	host, err := p.Join("u_host", "d_host", "Host", nil)
	p.Unlock()
	if err != nil {
		t.Fatalf("join host: %v", err)
	}
	return p, host
}

// dialParty opens a socket as the member and returns once the welcome frame has
// arrived. The read deadline is cleared so the caller controls how long to wait.
func dialParty(t *testing.T, ts *httptest.Server, code, token string) *websocket.Conn {
	t.Helper()
	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	u.Path = fmt.Sprintf("/ws/parties/%s", code)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)
	ws, _, err := websocket.DefaultDialer.Dial(u.String(), headers)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		var f map[string]interface{}
		if err := ws.ReadJSON(&f); err != nil {
			t.Fatalf("no welcome frame: %v", err)
		}
		if f["type"] == protocol.FrameWelcome {
			break
		}
	}
	_ = ws.SetReadDeadline(time.Time{})
	return ws
}

func isTimeout(err error) bool {
	nerr, ok := err.(net.Error)
	return ok && nerr.Timeout()
}

// Leaving must drop the device's socket. Before, the device stayed attached and
// kept receiving the party's state.
func TestLeaveClosesTheSocket(t *testing.T) {
	ts := setupTestServer()
	defer ts.Close()

	p, _ := newTestParty(t)
	p.Lock()
	guest, err := p.Join("u_guest", "d_guest", "Guest", nil)
	p.Unlock()
	if err != nil {
		t.Fatalf("join guest: %v", err)
	}
	ws := dialParty(t, ts, p.Code, guest.Token)
	defer ws.Close()

	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/parties/%s/leave", ts.URL, p.Code), nil)
	req.Header.Set("Authorization", "Bearer "+guest.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("leave failed: status %v, err %v", res, err)
	}
	res.Body.Close()

	// The leaving device hears why it was removed, then the socket closes.
	sawBye := false
	for {
		_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
		var f map[string]interface{}
		if err := ws.ReadJSON(&f); err != nil {
			if isTimeout(err) {
				t.Fatalf("socket still open after leave")
			}
			break
		}
		if f["type"] == protocol.FrameBye {
			sawBye = true
		}
	}
	if !sawBye {
		t.Errorf("leaving device was not told it was removed")
	}
}

// A member removed without its socket being closed (the expiry sweep does this)
// must not be able to act through that socket.
func TestRemovedMemberCannotActThroughAnOpenSocket(t *testing.T) {
	ts := setupTestServer()
	defer ts.Close()

	p, _ := newTestParty(t)
	p.Lock()
	guest, err := p.Join("u_guest", "d_guest", "Guest", nil)
	p.Unlock()
	if err != nil {
		t.Fatalf("join guest: %v", err)
	}
	ws := dialParty(t, ts, p.Code, guest.Token)
	defer ws.Close()

	p.Lock()
	p.Remove(guest.MemberId)
	p.Unlock()

	_ = ws.WriteJSON(map[string]interface{}{
		"type":   protocol.FrameControl,
		"action": protocol.ActionQueueAdd,
		"track":  map[string]interface{}{"videoId": "ghost", "title": "Ghost"},
	})

	// The server closes the socket after rejecting the frame. Wait for that.
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		var f map[string]interface{}
		if err := ws.ReadJSON(&f); err != nil {
			if isTimeout(err) {
				t.Fatalf("removed member's socket was not closed")
			}
			break
		}
	}

	p.Lock()
	queued := len(p.Playback.Queue)
	p.Unlock()
	if queued != 0 {
		t.Errorf("removed member changed the queue (%d tracks)", queued)
	}
}

// A reconnect replaces the socket. The replaced socket's cleanup must not mark
// the member offline, or empty the party, while the new socket is live.
func TestReconnectKeepsTheMemberOnline(t *testing.T) {
	ts := setupTestServer()
	defer ts.Close()

	p, host := newTestParty(t)

	first := dialParty(t, ts, p.Code, host.Token)
	defer first.Close()

	// The server closes the first socket when the second attaches.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			var f map[string]interface{}
			if err := first.ReadJSON(&f); err != nil {
				return
			}
		}
	}()

	second := dialParty(t, ts, p.Code, host.Token)
	defer second.Close()

	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the replaced socket was not closed")
	}
	// The replaced socket's cleanup runs after it closes. Give it time.
	time.Sleep(300 * time.Millisecond)

	p.Lock()
	connected := host.Connected
	empty := p.EmptySinceMs != nil
	p.Unlock()
	if !connected {
		t.Errorf("member shown offline while its replacement socket is connected")
	}
	if empty {
		t.Errorf("party marked empty while a member is connected")
	}
}
