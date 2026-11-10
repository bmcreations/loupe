package main

import (
	"bufio"
	"net"
	"testing"
	"time"
)

// A browser loads the page over TCP and then opens WebTransport to the same
// host:port over UDP. With -tcp the shaper's port answers both, so the page
// reaches the server unshaped and the stream it opens goes through the plan.
func TestTCPPassesThroughToTheServer(t *testing.T) {
	srv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() {
		c, err := srv.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		c.Write([]byte("echo " + line))
	}()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go passTCP(ln, srv.Addr().String())
	defer ln.Close()

	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo hello\n" {
		t.Fatalf("got %q through the relay, want %q", got, "echo hello\n")
	}
}
