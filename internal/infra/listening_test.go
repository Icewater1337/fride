package infra

import (
	"net"
	"testing"
)

// Adoption used to key only off docker's published ports, so a service run
// straight from a checkout — a dev server in the main checkout is the everyday
// case — was invisible, and compose was asked to bind a port already taken.
func TestListeningSeesNonContainerListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	if !Listening(port) {
		t.Errorf("listening(%d) = false while a listener is bound", port)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if Listening(port) {
		t.Errorf("listening(%d) = true after the listener closed", port)
	}
}
