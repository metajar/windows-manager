//go:build windows

package main

import (
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// pipeSDDL controls who may talk on the brain's named pipe:
//   - SYSTEM (SY) and the Administrators group (BA): full control (the brain
//     runs as SYSTEM).
//   - Authenticated Users (AU): read + write, so the face — which runs in the
//     logged-in kid's session — can connect, read state, and submit PINs.
//
// The face never receives the API token over this channel, only lock state, so
// a user connecting buys nothing beyond what the UI already shows.
const pipeSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;AU)"

// listenPipe creates the brain's pipe server.
func listenPipe() (net.Listener, error) {
	return winio.ListenPipe(pipeName, &winio.PipeConfig{
		SecurityDescriptor: pipeSDDL,
		MessageMode:        false,
	})
}

// dialPipe connects the face to the brain, waiting up to timeout.
func dialPipe(timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(pipeName, &timeout)
}
