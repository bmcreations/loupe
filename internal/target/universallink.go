package target

import (
	"bytes"
	"context"
	"debug/macho"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// This file is the pure half of opening a universal link on a simulator:
// which installed app claims an https URL. iOS answers that in swcd, which a
// process loupe spawns on the simulator cannot reach, so loupe works it out
// the way swcd does: the app's associated domains name the host, and the
// host's apple-app-site-association names the app and the path.

// appLinkEntitlements is what of an app's entitlements decides whether it
// can take a universal link: its appID (team prefix and bundle ID) and its
// applinks: domains, each without the "applinks:" prefix.
type appLinkEntitlements struct {
	AppID   string
	Domains []string
}

// machoEntitlements reads the entitlements plist from an executable's
// __TEXT,__entitlements section. A simulator build keeps them there, where
// the simulator reads them, rather than in the code signature, which holds
// none. A fat binary's first slice is read: every slice carries the same.
func machoEntitlements(path string) ([]byte, error) {
	f, err := macho.Open(path)
	if err != nil {
		fat, ferr := macho.OpenFat(path)
		if ferr != nil {
			return nil, err
		}
		defer fat.Close()
		if len(fat.Arches) == 0 {
			return nil, errors.New("empty fat binary")
		}
		return sectionData(fat.Arches[0].File)
	}
	defer f.Close()
	return sectionData(f)
}

func sectionData(f *macho.File) ([]byte, error) {
	for _, s := range f.Sections {
		if s.Seg == "__TEXT" && s.Name == "__entitlements" {
			b, err := s.Data()
			return bytes.TrimRight(b, "\x00"), err
		}
	}
	return nil, nil
}

// parseAppLinkEntitlements reads application-identifier and the applinks:
// entries of com.apple.developer.associated-domains from an XML plist, and
// skips everything else in it.
func parseAppLinkEntitlements(b []byte) (appLinkEntitlements, error) {
	var e appLinkEntitlements
	d := xml.NewDecoder(bytes.NewReader(b))
	depth := 0 // dict and array nesting; the keys wanted are at depth 1
	key := ""
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return e, nil
		}
		if err != nil {
			return e, fmt.Errorf("entitlements: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				var s string
				if err := d.DecodeElement(&s, &t); err != nil {
					return e, err
				}
				if depth == 1 {
					key = s
				}
				continue
			case "string":
				var s string
				if err := d.DecodeElement(&s, &t); err != nil {
					return e, err
				}
				switch {
				case depth == 1 && key == "application-identifier":
					e.AppID = s
				case depth == 2 && key == "com.apple.developer.associated-domains":
					if host, ok := strings.CutPrefix(s, "applinks:"); ok {
						e.Domains = append(e.Domains, host)
					}
				}
				continue
			case "dict", "array":
				depth++
				continue
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "dict", "array":
				depth--
				if depth == 1 {
					// A top-level value has ended, so its key no longer
					// applies to what comes next.
					key = ""
				}
			}
		}
	}
}

// claimsHost reports whether an applinks: entry claims host. An entry may
// end in "?mode=developer" or similar, which is not part of the host.
// "*.example.com" claims every subdomain of example.com but not
// example.com itself, and the file read is the subdomain's own. Hosts
// compare without case.
func claimsHost(entry, host string) bool {
	entry, _, _ = strings.Cut(entry, "?")
	entry = strings.ToLower(entry)
	host = strings.ToLower(host)
	if base, ok := strings.CutPrefix(entry, "*."); ok {
		return strings.HasSuffix(host, "."+base)
	}
	return entry == host
}

// aasaFile is an apple-app-site-association file's applinks, in either
// form: details with appIDs and components (iOS 13 on), or with an appID
// and paths (before). A detail may carry both.
type aasaFile struct {
	AppLinks struct {
		Details []aasaDetail `json:"details"`
	} `json:"applinks"`
}

type aasaDetail struct {
	AppIDs     []string        `json:"appIDs"`
	AppID      string          `json:"appID"`
	Components []aasaComponent `json:"components"`
	Paths      []string        `json:"paths"`
}

// aasaComponent is one entry of a detail's components. A field that is
// absent matches anything. Query is either a string, matched against the
// whole query, or an object of names to value patterns.
type aasaComponent struct {
	Path          *string         `json:"/"`
	Query         json.RawMessage `json:"?"`
	Fragment      *string         `json:"#"`
	Exclude       bool            `json:"exclude"`
	CaseSensitive *bool           `json:"caseSensitive"`
}

// aasaClaims reports whether file gives u to the app appID. The details
// naming appID are read in order, and the first component or path that
// matches decides: an exclude is a no, anything else a yes. Nothing
// matching is a no.
func aasaClaims(file []byte, appID string, u *url.URL) (bool, error) {
	var f aasaFile
	if err := json.Unmarshal(file, &f); err != nil {
		return false, fmt.Errorf("apple-app-site-association: %w", err)
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	for _, d := range f.AppLinks.Details {
		if !detailNames(d, appID) {
			continue
		}
		for _, c := range d.Components {
			if c.matches(p, u) {
				return !c.Exclude, nil
			}
		}
		for _, pat := range d.Paths {
			pat, not := strings.CutPrefix(pat, "NOT ")
			if globMatch(pat, p, true) {
				return !not, nil
			}
		}
	}
	return false, nil
}

func detailNames(d aasaDetail, appID string) bool {
	if d.AppID == appID {
		return true
	}
	for _, id := range d.AppIDs {
		if id == appID {
			return true
		}
	}
	return false
}

func (c aasaComponent) matches(path string, u *url.URL) bool {
	cs := c.CaseSensitive == nil || *c.CaseSensitive
	if c.Path != nil && !globMatch(*c.Path, path, cs) {
		return false
	}
	if c.Fragment != nil && !globMatch(*c.Fragment, u.EscapedFragment(), cs) {
		return false
	}
	if len(c.Query) == 0 || string(c.Query) == "null" {
		return true
	}
	var whole string
	if json.Unmarshal(c.Query, &whole) == nil {
		return globMatch(whole, u.RawQuery, cs)
	}
	var items map[string]string
	if err := json.Unmarshal(c.Query, &items); err != nil {
		return false
	}
	q := u.Query()
	for name, pat := range items {
		vals, ok := q[name]
		if !ok {
			return false
		}
		hit := false
		for _, v := range vals {
			if globMatch(pat, v, cs) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// globMatch is an apple-app-site-association pattern: "*" matches any run
// of characters, "/" included, and "?" exactly one. path.Match will not do,
// since its "*" stops at a slash and "/chat/*" must take "/chat/a/b".
func globMatch(pat, s string, caseSensitive bool) bool {
	if !caseSensitive {
		pat, s = strings.ToLower(pat), strings.ToLower(s)
	}
	pr, sr := []rune(pat), []rune(s)
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(sr) {
		switch {
		case pi < len(pr) && (pr[pi] == '?' || pr[pi] == sr[si]):
			pi++
			si++
		case pi < len(pr) && pr[pi] == '*':
			star, mark = pi, si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(pr) && pr[pi] == '*' {
		pi++
	}
	return pi == len(pr)
}

// aasaMaxBytes is iOS's own limit on the file.
const aasaMaxBytes = 128 << 10

const (
	aasaTTL     = 5 * time.Minute
	aasaFailTTL = 30 * time.Second
)

var (
	aasaMu    sync.Mutex
	aasaCache = map[string]aasaEntry{}
)

type aasaEntry struct {
	body []byte
	err  error
	at   time.Time
}

// aasaFor is host's apple-app-site-association, cached for a few minutes,
// and a failure for less. It is a var so a test never reaches the network.
var aasaFor = func(ctx context.Context, host string) ([]byte, error) {
	host = strings.ToLower(host)
	aasaMu.Lock()
	e, ok := aasaCache[host]
	aasaMu.Unlock()
	if ok {
		ttl := aasaTTL
		if e.err != nil {
			ttl = aasaFailTTL
		}
		if time.Since(e.at) < ttl {
			return e.body, e.err
		}
	}
	body, err := fetchAASA(ctx, host)
	aasaMu.Lock()
	aasaCache[host] = aasaEntry{body: body, err: err, at: time.Now()}
	aasaMu.Unlock()
	return body, err
}

var aasaClient = &http.Client{Timeout: 5 * time.Second}

// fetchAASA asks Apple's CDN first, which is where iOS gets the file, and
// the host itself after. Neither URL carries anything of the link but its
// host.
func fetchAASA(ctx context.Context, host string) ([]byte, error) {
	var errs []error
	for _, src := range []string{
		"https://app-site-association.cdn-apple.com/a/v1/" + host,
		"https://" + host + "/.well-known/apple-app-site-association",
	} {
		b, err := getAASA(ctx, src)
		if err == nil {
			return b, nil
		}
		errs = append(errs, err)
	}
	return nil, fmt.Errorf("apple-app-site-association for %s: %w", host, errors.Join(errs...))
}

func getAASA(ctx context.Context, src string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	resp, err := aasaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", src, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, aasaMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > aasaMaxBytes {
		return nil, fmt.Errorf("%s: over %d bytes", src, aasaMaxBytes)
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("%s: not JSON", src)
	}
	return b, nil
}
