package main

import (
	"net"
	"testing"
	"time"
)

func TestIPCRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	reader := newMessageReader(c2)
	want := []ipcMessage{
		stateMessage(true, 1234, true),
		pinMessage("4242"),
		pinResultMessage(true),
		stateMessage(false, 0, false),
	}

	go func() {
		for _, m := range want {
			if err := writeMessage(c1, m); err != nil {
				t.Errorf("write: %v", err)
				return
			}
		}
	}()

	for _, exp := range want {
		c2.SetReadDeadline(time.Now().Add(2 * time.Second))
		got, err := reader.read()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got != exp {
			t.Fatalf("got %+v, want %+v", got, exp)
		}
	}
}

func TestIPCMessageConstructors(t *testing.T) {
	if m := stateMessage(true, 60, true); m.Type != msgState || !m.Locked || m.Remaining != 60 || !m.Online {
		t.Fatalf("stateMessage wrong: %+v", m)
	}
	if m := pinMessage("1234"); m.Type != msgPIN || m.PIN != "1234" {
		t.Fatalf("pinMessage wrong: %+v", m)
	}
	if m := pinResultMessage(true); m.Type != msgPINResult || !m.OK {
		t.Fatalf("pinResultMessage wrong: %+v", m)
	}
}
