package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bmcreations/loupe/internal/httpapi"
	"github.com/bmcreations/loupe/internal/mcpserver"
	qrcode "github.com/skip2/go-qrcode"
)

// adminClient talks to a running server with the master token, the way
// `loupe mcp` does: the token comes from -token, LOUPE_TOKEN or the token
// file, and a loopback server's certificate is not checked.
type adminClient struct {
	base  *url.URL
	token string
	http  *http.Client
	// certName is the first DNS name on the server's certificate, from the
	// last answer: the name a device off this machine reaches it by.
	certName string
}

func newAdminClient(rawURL, tokenFlag string) (*adminClient, error) {
	tok, err := mcpToken(tokenFlag)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("server url %q: want https:// or http://", rawURL)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if h := u.Hostname(); h == "localhost" || (net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback()) {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &adminClient{base: u, token: tok, http: &http.Client{Transport: tr, Timeout: 15 * time.Second}}, nil
}

// do sends a request and decodes a 2xx JSON answer into out, which may be
// nil. A non-2xx answer is an error carrying the server's message.
func (c *adminClient) do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base.String()+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// /api/pair takes no credential; sending one anyway is harmless.
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		if mcpserver.PlainHTTPAnswered(c.base, err) {
			c.base.Scheme = "http"
			return c.do(method, path, body, out)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 && len(resp.TLS.PeerCertificates[0].DNSNames) > 0 {
		c.certName = resp.TLS.PeerCertificates[0].DNSNames[0]
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound && strings.HasPrefix(path, "/api/") && !bytes.Contains(raw, []byte(`"error"`)) {
		return errors.New("this server does not support pairing; update it")
	}
	if resp.StatusCode/100 != 2 {
		var e httpapi.ErrorResponse
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s %s: %d: %s", method, path, resp.StatusCode, e.Error)
		}
		return fmt.Errorf("%s %s: %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// runPair is `loupe pair`: mint a pairing code for a person to type on a new
// device, or with -bearer, mint one and redeem it at once for a client that
// cannot open a link, printing the session token.
func runPair(args []string) int {
	fs := flag.NewFlagSet("loupe pair", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var targets stringList
	fs.Var(&targets, "target", "a target the paired device should open (repeatable)")
	var (
		addr    = fs.String("url", "https://localhost:18456", "the loupe server")
		token   = fs.String("token", "", "master token (default: LOUPE_TOKEN, then the server's token file)")
		label   = fs.String("label", "", "a name for the paired device, shown in `loupe sessions`")
		canPair = fs.Bool("can-pair", false, "let the paired device mint pairing codes of its own")
		bearerF = fs.Bool("bearer", false, "redeem the code here and print a bearer session token instead")
		hostF   = fs.String("host", "", "the name a phone reaches the server by, for the link (default: the name on its certificate)")
		pngF    = fs.String("png", "", "also write the link's QR code to this PNG file")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, err := newAdminClient(*addr, *token)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loupe pair:", err)
		return 1
	}
	var code httpapi.PairingResponse
	if err := c.do(http.MethodPost, "/api/pairings", httpapi.PairingRequest{Target: targets, Label: *label, CanPair: *canPair}, &code); err != nil {
		fmt.Fprintln(os.Stderr, "loupe pair:", err)
		return 1
	}
	if *bearerF {
		var sess httpapi.PairResponse
		if err := c.do(http.MethodPost, "/api/pair", httpapi.PairRequest{Code: code.Code, Kind: "bearer", Label: *label}, &sess); err != nil {
			fmt.Fprintln(os.Stderr, "loupe pair:", err)
			return 1
		}
		// The token alone on stdout, so it can be captured; the rest is for
		// the person reading.
		fmt.Fprintf(os.Stderr, "session %s; send it as Authorization: Bearer <token>. It expires 30 days after its last use.\n", sess.ID)
		fmt.Println(sess.Token)
		return 0
	}
	host := *hostF
	if host == "" {
		host = c.certName
	}
	if host == "" || strings.HasPrefix(host, "*.") {
		host = c.base.Hostname()
	}
	port := c.base.Port()
	if port == "" {
		port = "443"
	}
	// The QR uses the app's scheme, so Camera opens Handset; the text link
	// is the same one over https, for pasting.
	scanned := pairLink(handsetScheme, host, port, targets, code.Code)
	fmt.Printf("link: %s\n", pairLink("https", host, port, targets, code.Code))
	if qr, err := bannerQR(scanned); err == nil {
		fmt.Print(qr)
	}
	if *pngF != "" {
		if err := qrcode.WriteFile(scanned, qrcode.Medium, 512, *pngF); err != nil {
			fmt.Fprintln(os.Stderr, "loupe pair: writing the QR:", err)
			return 1
		}
		fmt.Printf("qr: %s\n", *pngF)
	}
	fmt.Printf("code: %s\n", code.Code)
	fmt.Printf("expires: %s (in %s); it works once\n", code.ExpiresAt.Local().Format(time.Kitchen), time.Until(code.ExpiresAt).Round(time.Second))
	fmt.Printf("redeem: POST %s/api/pair {\"code\": \"%s\", \"kind\": \"browser\"|\"bearer\"|\"peer\"}\n", c.base, code.Code)
	return 0
}

// handsetScheme is the URL scheme Handset registers for pairing links.
const handsetScheme = "loupe-handset"

// pairLink is the link Handset pairs from: the server, any targets to open,
// and the code in the fragment, which never reaches a server log.
func pairLink(scheme, host, port string, targets []string, code string) string {
	u := url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port), Path: "/pair", Fragment: "code=" + code}
	if len(targets) > 0 {
		u.RawQuery = url.Values{"target": targets}.Encode()
	}
	return u.String()
}

// runSessions is `loupe sessions`: list them, or revoke one or all.
func runSessions(args []string) int {
	fs := flag.NewFlagSet("loupe sessions", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		addr  = fs.String("url", "https://localhost:18456", "the loupe server")
		token = fs.String("token", "", "master token (default: LOUPE_TOKEN, then the server's token file)")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: loupe sessions [-url u] [-token t]\n       loupe sessions [-url u] revoke <id> | -all")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, err := newAdminClient(*addr, *token)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loupe sessions:", err)
		return 1
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return listSessions(c)
	}
	if rest[0] != "revoke" || len(rest) != 2 {
		fs.Usage()
		return 2
	}
	var out httpapi.RevokeResponse
	path := "/api/sessions/" + url.PathEscape(rest[1])
	if rest[1] == "-all" {
		// With the master token there is no caller's session to keep.
		path = "/api/sessions?others=1"
	}
	if err := c.do(http.MethodDelete, path, nil, &out); err != nil {
		fmt.Fprintln(os.Stderr, "loupe sessions:", err)
		return 1
	}
	if len(out.Revoked) == 0 {
		fmt.Println("nothing to revoke")
		return 0
	}
	fmt.Printf("revoked %s\n", strings.Join(out.Revoked, ", "))
	return 0
}

func listSessions(c *adminClient) int {
	var rows []httpapi.SessionInfo
	if err := c.do(http.MethodGet, "/api/sessions", nil, &rows); err != nil {
		fmt.Fprintln(os.Stderr, "loupe sessions:", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Println("no sessions; pair a device with `loupe pair`")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tLABEL\tPAIRED BY\tCAN PAIR\tLAST USED\tFROM\tSTATE")
	for _, r := range rows {
		state := "expires " + r.ExpiresAt.Local().Format("2006-01-02")
		if r.RevokedAt != nil {
			state = "revoked " + r.RevokedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\t%s\t%s\t%s\n", r.ID, r.Kind, dash(r.Label), r.Parent,
			r.CanPair, r.LastUsedAt.Local().Format("2006-01-02 15:04"), dash(r.RemoteAddr), state)
	}
	_ = tw.Flush()
	return 0
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
