package main

import (
	"io"
	"log"
	"net"
)

// passTCP relays every TCP connection on ln to the server, unshaped. A browser
// viewer loads the page over TCP and then dials WebTransport at the page's own
// host:port, so the shaper's port has to serve the page too. Only the UDP side
// (the stream) is the link under test; the page and its API calls are not.
func passTCP(ln net.Listener, to string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			s, err := net.Dial("tcp", to)
			if err != nil {
				log.Println("tcp:", err)
				return
			}
			defer s.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(s, c); done <- struct{}{} }()
			go func() { io.Copy(c, s); done <- struct{}{} }()
			<-done
		}()
	}
}
