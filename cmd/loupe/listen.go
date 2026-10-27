package main

import (
	"fmt"
	"net"
	"strings"
)

// checkPlainHTTP refuses to serve plain HTTP anywhere but loopback. Over
// http the cookie cannot be Secure, and the token, the /ws stream and the
// clipboard text cross the network in the clear, readable by anyone on the
// path. Loopback stays allowed: nothing leaves the machine, and spare test
// servers bind 127.0.0.1 without a certificate.
func checkPlainHTTP(addr string, useTLS, insecure bool) error {
	if useTLS || insecure || loopbackOnly(addr) {
		return nil
	}
	return fmt.Errorf("%s is reachable from other machines, and without TLS the token and "+
		"everything streamed cross the network in the clear. Give -tls-cert and -tls-key, "+
		"bind loopback (-addr 127.0.0.1%s), or pass -insecure-http to serve plain HTTP anyway",
		addr, portOf(addr))
}

// loopbackOnly reports whether addr binds only a loopback interface. An empty
// host, as in ":8080", is every interface.
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return ":" + port
	}
	return ""
}
