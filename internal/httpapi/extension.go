package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Extensions are separate programs that talk to loupe over HTTP. One that has
// something for viewers to read registers here, and loupe passes
// /api/ext/{name}/... through to it, so a client reads an extension's data
// while only ever talking to loupe, with loupe's token, the way overlays work
// for what goes over the video. loupe never reads what passes through: a
// client that knows the extension knows its shape.
//
// Only loopback URLs register. Anyone with the token can register, and a
// pass-through to any other host would let them use loupe to reach it.

const (
	// extensionTTL is how long a registration lasts without being renewed.
	// An extension re-registers well inside it, so one that stops without
	// saying so drops out of the list on its own.
	extensionTTL = 90 * time.Second
	// maxExtensions bounds the registry.
	maxExtensions = 32
)

var extensionNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ExtensionRequest is POST /api/extensions's body.
type ExtensionRequest struct {
	// Name is the path segment clients reach it under, /api/ext/{name}/.
	Name string `json:"name"`
	// URL is where the extension listens: http or https on 127.0.0.1, ::1
	// or localhost.
	URL string `json:"url"`
	// Label is a name to show a person, defaulting to Name.
	Label string `json:"label,omitempty"`
	// Page, when set, is a path under URL that the web client opens as the
	// extension's page, such as "/". See validPage for its rules.
	Page string `json:"page,omitempty"`
}

// Extension is one registered extension.
type Extension struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// Path is where clients reach it, "/api/ext/{name}/".
	Path string `json:"path"`
	// Page is the request's Page as a loupe path: "/api/ext/{name}/" for a
	// page of "/". Empty, and left out, when the extension has no page.
	Page string `json:"page,omitempty"`
}

// ExtensionsResponse is GET /api/extensions's body.
type ExtensionsResponse struct {
	Extensions []Extension `json:"extensions"`
}

type registeredExtension struct {
	Extension
	target  *url.URL
	expires time.Time
	proxy   *httputil.ReverseProxy
}

type extensionRegistry struct {
	mu  sync.Mutex
	all map[string]*registeredExtension
	now func() time.Time
}

func newExtensionRegistry() *extensionRegistry {
	return &extensionRegistry{all: make(map[string]*registeredExtension), now: time.Now}
}

// loopbackURL parses raw and accepts it only when its host is this machine.
func loopbackURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("url must be an absolute http or https URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("url must not carry credentials, a query or a fragment")
	}
	host := u.Hostname()
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, errors.New("url must be on 127.0.0.1, ::1 or localhost")
		}
	}
	return u, nil
}

// maxPageLen bounds an extension's page path.
const maxPageLen = 200

// validPage checks an extension's page path. The web client loads it in a
// frame on loupe's own origin, so it has to stay a plain path under the
// extension's prefix: nothing that climbs out of it, once decoded, or that
// a browser reads as a query, a fragment or a separator.
func validPage(page string) error {
	if len(page) > maxPageLen {
		return errors.New("page must be at most 200 characters")
	}
	if !strings.HasPrefix(page, "/") {
		return errors.New(`page must start with "/"`)
	}
	decoded, err := url.PathUnescape(page)
	if err != nil {
		return errors.New("page is not a valid path: " + err.Error())
	}
	for _, p := range []string{page, decoded} {
		if strings.Contains(p, "..") || strings.ContainsAny(p, "?#\\") {
			return errors.New(`page must not contain "..", "?", "#" or a backslash`)
		}
		for _, c := range p {
			if c < 0x20 || c == 0x7f {
				return errors.New("page must not contain control characters")
			}
		}
	}
	return nil
}

func (x *extensionRegistry) register(req ExtensionRequest) (Extension, error) {
	if !extensionNamePattern.MatchString(req.Name) {
		return Extension{}, errors.New("name must be lowercase letters, digits and dashes, at most 32")
	}
	u, err := loopbackURL(req.URL)
	if err != nil {
		return Extension{}, err
	}
	if req.Page != "" {
		if err := validPage(req.Page); err != nil {
			return Extension{}, err
		}
	}
	label := req.Label
	if label == "" {
		label = req.Name
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.expireLocked()
	if _, ok := x.all[req.Name]; !ok && len(x.all) >= maxExtensions {
		return Extension{}, errors.New("too many extensions registered")
	}
	ext := Extension{Name: req.Name, Label: label, Path: "/api/ext/" + req.Name + "/"}
	if req.Page != "" {
		ext.Page = "/api/ext/" + req.Name + req.Page
	}
	e := &registeredExtension{
		Extension: ext,
		target:    u,
		expires:   x.now().Add(extensionTTL),
	}
	e.proxy = x.newProxy(e)
	x.all[req.Name] = e
	return e.Extension, nil
}

func (x *extensionRegistry) expireLocked() {
	now := x.now()
	for name, e := range x.all {
		if now.After(e.expires) {
			delete(x.all, name)
		}
	}
}

func (x *extensionRegistry) list() []Extension {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.expireLocked()
	out := make([]Extension, 0, len(x.all))
	for _, e := range x.all {
		out = append(out, e.Extension)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (x *extensionRegistry) get(name string) *registeredExtension {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.expireLocked()
	return x.all[name]
}

func (x *extensionRegistry) remove(name string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	_, ok := x.all[name]
	delete(x.all, name)
	return ok
}

// dropIf removes name only if it is still e, so a failed request to an old
// registration cannot drop the one that replaced it.
func (x *extensionRegistry) dropIf(name string, e *registeredExtension) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.all[name] == e {
		delete(x.all, name)
	}
}

// newProxy passes a request through to e with loupe's own credentials taken
// off: the extension has its own token and has no need of the client's.
func (x *extensionRegistry) newProxy(e *registeredExtension) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			rest := strings.TrimPrefix(r.In.URL.Path, e.Path)
			r.Out.URL.Scheme = e.target.Scheme
			r.Out.URL.Host = e.target.Host
			r.Out.URL.Path = strings.TrimSuffix(e.target.Path, "/") + "/" + rest
			r.Out.URL.RawPath = ""
			q := r.In.URL.Query()
			q.Del("token")
			r.Out.URL.RawQuery = q.Encode()
			r.Out.Host = e.target.Host
			r.Out.Header.Del("Authorization")
			r.Out.Header.Del("Cookie")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// It stopped without saying so; it re-registers when it is back.
			x.dropIf(e.Name, e)
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: "extension " + e.Name + " is not answering: " + err.Error()})
		},
	}
}

// handleExtensions lists, registers or removes extensions.
//
//	GET    /api/extensions
//	POST   /api/extensions  {"name", "url", "label", "page"}
//	DELETE /api/extensions?name=
func (s *Server) handleExtensions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, http.StatusOK, ExtensionsResponse{Extensions: s.exts.list()})
	case http.MethodPost:
		// Unknown fields are ignored, not refused, so an extension that sends
		// a field a newer loupe added still registers with an older one.
		var req ExtensionRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "bad body: " + err.Error()})
			return
		}
		e, err := s.exts.register(req)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, e)
	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		if !s.exts.remove(name) {
			writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "no extension " + name})
			return
		}
		writeJSON(w, http.StatusOK, OKResponse{OK: true})
	}
}

// handleExtensionProxy passes /api/ext/{name}/... through to the extension.
func (s *Server) handleExtensionProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/ext/")
	name, _, _ := strings.Cut(rest, "/")
	e := s.exts.get(name)
	if e == nil {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "no extension " + name})
		return
	}
	if !strings.HasPrefix(r.URL.Path, e.Path) {
		http.Redirect(w, r, e.Path, http.StatusMovedPermanently)
		return
	}
	e.proxy.ServeHTTP(w, r)
}
