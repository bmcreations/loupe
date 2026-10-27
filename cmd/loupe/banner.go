package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bmcreations/loupe/internal/httpapi"
	scrcpyserver "github.com/bmcreations/loupe/internal/scrcpy/server"
)

// banner is what the server prints once it has settled where it listens.
type banner struct {
	addr, scheme string
	// handoff is the phone link, and carries the token when there is one.
	handoff    string
	advertised []string
	token      string
	// tokenWhere says where the token can be read when the banner holds it
	// back.
	tokenWhere   string
	webTransport bool
	tls          bool
}

// write prints the banner. showToken is whether the output is a terminal:
// under launchd it is loupe-run.log, a file that outlives the run, and the
// token printed into it would be readable for as long as the file is. The
// QR code encodes the phone link, so it is a copy of the token too and is
// held back with it.
func (b banner) write(w io.Writer, showToken bool) {
	secret := b.token != ""
	fmt.Fprintf(w, "\nloupe listening on %s (%s)\n", b.addr, b.scheme)
	if u := b.handoff; u != "" && (showToken || !secret) {
		if code, err := bannerQR(u); err == nil {
			fmt.Fprintf(w, "  phone: %s\n%s", u, code)
		}
	}
	for _, u := range b.advertised {
		if secret && showToken {
			u += "?token=" + b.token
		}
		fmt.Fprintf(w, "  %s\n", u)
	}
	if secret {
		if showToken {
			fmt.Fprintf(w, "  agent: Authorization: Bearer %s\n", b.token)
		} else {
			fmt.Fprintf(w, "  token: not printed, since this output is not a terminal; it is in %s\n", b.tokenWhere)
			fmt.Fprintln(w, "  phone: GET /api/handoff with the token returns the phone link")
		}
	}
	if b.webTransport {
		fmt.Fprintf(w, "  webtransport: https://<host>%s/wt (HTTP/3 over UDP)\n", b.addr)
		fmt.Fprintf(w, "  native: QUIC ALPN %q on the same port\n", httpapi.QUICALPN)
	}
	if b.tls {
		// Terminating TLS here rather than behind `tailscale serve` is not
		// cosmetic. A reverse proxy accepts writes over loopback at
		// effectively infinite speed and then buffers them for the slow
		// hop, so this process sees a healthy socket while seconds of video
		// queue up somewhere it cannot observe or cancel. One hop means the
		// send buffer this server controls is the link that is actually
		// slow.
		fmt.Fprintln(w, "  (direct TLS: no proxy between this server and the link)")
	}
	fmt.Fprintln(w)
}

// tokenWhere names where resolveToken found the token, in the same order.
func tokenWhere(flagValue string) string {
	if flagValue != "" {
		return "the -token flag"
	}
	if os.Getenv("LOUPE_TOKEN") != "" {
		return "LOUPE_TOKEN"
	}
	return filepath.Join(scrcpyserver.DefaultDir(), tokenFileName)
}

// isTerminal reports whether f is a terminal rather than a file or a pipe.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// tightenOutput makes f owner-only when it is a regular file. launchd opens
// loupe-run.log before anything here runs, with its own umask, and the log
// holds device names, paths and, from older builds, the token.
func tightenOutput(f *os.File) {
	if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
		_ = f.Chmod(0o600)
	}
}
