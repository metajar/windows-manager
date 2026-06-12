package main

import (
	"encoding/json"
	"io"
)

// IPC between the brain (the SYSTEM service) and the face (the per-session UI
// agent) is newline-delimited JSON over a named pipe. The protocol is tiny and
// lives here, free of any Windows specifics, so it can be unit-tested over an
// in-memory net.Pipe on any platform.
//
// The brain pushes state messages whenever lock state changes and replies to
// each PIN attempt with a pinResult. Crucially, the face never sees the server
// token: it just renders state and forwards PIN attempts, and the brain does
// the privileged talking. That shrinks the attack surface on the kid's box.

// pipeName is the named pipe the brain serves and the face dials.
const pipeName = `\\.\pipe\rewardd-agent`

type msgType string

const (
	msgState     msgType = "state"     // brain -> face: current lock state
	msgPIN       msgType = "pin"       // face -> brain: a PIN attempt
	msgPINResult msgType = "pinresult" // brain -> face: accepted or not
)

// ipcMessage is the single envelope for every direction. Fields not relevant to
// a given Type are omitted on the wire.
type ipcMessage struct {
	Type      msgType `json:"type"`
	Locked    bool    `json:"locked,omitempty"`
	Remaining int64   `json:"remaining,omitempty"`
	Online    bool    `json:"online,omitempty"`
	PIN       string  `json:"pin,omitempty"`
	OK        bool    `json:"ok,omitempty"`
}

func stateMessage(locked bool, remaining int64, online bool) ipcMessage {
	return ipcMessage{Type: msgState, Locked: locked, Remaining: remaining, Online: online}
}

func pinMessage(pin string) ipcMessage {
	return ipcMessage{Type: msgPIN, PIN: pin}
}

func pinResultMessage(ok bool) ipcMessage {
	return ipcMessage{Type: msgPINResult, OK: ok}
}

// writeMessage encodes one message followed by a newline.
func writeMessage(w io.Writer, m ipcMessage) error {
	return json.NewEncoder(w).Encode(m)
}

// messageReader streams messages off a connection. json.Decoder happily reads
// successive whitespace-separated objects, so this works for our newline framing.
type messageReader struct{ dec *json.Decoder }

func newMessageReader(r io.Reader) *messageReader {
	return &messageReader{dec: json.NewDecoder(r)}
}

func (mr *messageReader) read() (ipcMessage, error) {
	var m ipcMessage
	err := mr.dec.Decode(&m)
	return m, err
}
