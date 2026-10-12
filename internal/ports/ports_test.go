package ports

import (
	"net"
	"testing"
)

func TestFreeSeesABoundPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port

	// Bound on loopback only, yet Free must see it: the bind test uses the
	// wildcard address, which conflicts with any listener on the port.
	if Free(port) {
		t.Fatalf("Free(%d) = true while a listener holds it", port)
	}
	l.Close()
	if !Free(port) {
		t.Fatalf("Free(%d) = false after the listener closed", port)
	}
}

func TestFreeSeesAWildcardListener(t *testing.T) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if Free(l.Addr().(*net.TCPAddr).Port) {
		t.Fatal("Free = true on a port bound on every interface")
	}
}

func TestFreeReleasesWhatItBinds(t *testing.T) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	for range 3 { // a leaked bind would make the second call fail
		if !Free(port) {
			t.Fatalf("Free(%d) = false on a free port", port)
		}
	}
}

func TestFreeRejectsOutOfRange(t *testing.T) {
	for _, p := range []int{-1, 0, 65536} {
		if Free(p) {
			t.Errorf("Free(%d) = true", p)
		}
	}
}
