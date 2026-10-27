package main

import (
	"crypto/tls"
	"net/url"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// phoneURL is the one link a phone can open: its host is a name the TLS
// certificate covers, because the banner's other links are localhost and
// interface IPs, and a phone that scans an IP gets a certificate error.
// Without TLS any reachable address will do, so it falls back to the first
// advertised one that is not localhost. It returns "" when nothing fits,
// and the banner then prints no code rather than a wrong one.
func phoneURL(cert *tls.Certificate, useTLS bool, addr, token string, advertised []string) string {
	port := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		port = addr[i+1:]
	}
	var u string
	switch {
	case useTLS && cert != nil && cert.Leaf != nil && len(cert.Leaf.DNSNames) > 0:
		u = "https://" + cert.Leaf.DNSNames[0] + ":" + port + "/"
	case useTLS:
		return ""
	default:
		for _, a := range advertised {
			if !strings.Contains(a, "://localhost:") {
				u = a + "/"
				break
			}
		}
		if u == "" {
			return ""
		}
	}
	if token != "" {
		u += "?token=" + url.QueryEscape(token)
	}
	return u
}

// bannerQR draws u as a QR code in half blocks, two modules to a line, so a
// version 6 code with its quiet zone is about 25 lines and stays inside
// `make agent-log`'s 40. ECC L keeps the version, and so the module count,
// as low as the URL allows. Plain Unicode only: loupe-run.log is read as text.
func bannerQR(u string) (string, error) {
	q, err := qrcode.New(u, qrcode.Low)
	if err != nil {
		return "", err
	}
	return q.ToSmallString(false), nil
}
