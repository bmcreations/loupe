package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// extensionNamePattern is httpapi's extensionNamePattern, checked here so a
// bad name is refused before a request.
var extensionNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// extensionMethods are the methods /api/ext/ routes (httpapi.Routes). PATCH
// would reach the mux and come back 405, so it is refused here instead.
var extensionMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true, http.MethodDelete: true,
}

// maxExtensionBody bounds the body call_extension hands back.
const maxExtensionBody = 64 << 10

type callExtensionIn struct {
	Name   string `json:"name" jsonschema:"the extension's name, from list_extensions"`
	Method string `json:"method,omitempty" jsonschema:"GET (default), POST, PUT or DELETE"`
	Path   string `json:"path,omitempty" jsonschema:"path under /api/ext/{name}/, relative, with an optional query, e.g. builds?limit=5; empty is the extension's root"`
	Body   string `json:"body,omitempty" jsonschema:"request body as text, sent as application/json when it parses as JSON and text/plain otherwise"`
}

// extensionPath returns the path and query to request under /api/ext/{name}/,
// or an error when path could leave it.
func extensionPath(p string) (string, url.Values, error) {
	if strings.ContainsAny(p, "\x00\r\n\\") {
		return "", nil, fmt.Errorf("path has a control or backslash character")
	}
	raw, rawQuery, _ := strings.Cut(p, "?")
	if strings.HasPrefix(raw, "//") || strings.Contains(raw, "://") {
		return "", nil, fmt.Errorf("path must be relative to /api/ext/{name}/, not a URL")
	}
	dec, err := url.PathUnescape(raw)
	if err != nil {
		return "", nil, fmt.Errorf("path is not valid percent-encoding: %v", err)
	}
	// A second layer of encoding is no path anyone means; refuse it
	// rather than guess how far the extension decodes.
	if strings.ContainsAny(dec, "\\\x00") || strings.Contains(strings.ToLower(dec), "%2e") || strings.Contains(strings.ToLower(dec), "%2f") || strings.Contains(strings.ToLower(dec), "%5c") {
		return "", nil, fmt.Errorf("path has an encoded dot, slash or backslash")
	}
	dec = strings.TrimPrefix(dec, "/")
	if strings.HasPrefix(dec, "/") {
		return "", nil, fmt.Errorf("path must be relative to /api/ext/{name}/")
	}
	for _, seg := range strings.Split(dec, "/") {
		if seg == ".." || seg == "." {
			return "", nil, fmt.Errorf("path must not contain . or .. segments")
		}
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", nil, fmt.Errorf("bad query: %v", err)
	}
	return dec, q, nil
}

func addExtensionTools(s *mcp.Server, c *Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_extensions",
		Description: "List the extensions registered with loupe: name, label, path (their base path under /api/ext/) and page when they have one. Pass a name and path to call_extension to use one. Reads only; it claims nothing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return jsonResult(c.getJSON(ctx, "/api/extensions", nil))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "call_extension",
		Description: "Call a registered extension through loupe, at /api/ext/{name}/{path}, with this session's token (the extension never sees it). " +
			"The answer is the extension's status, content type and body as text, whatever the status: a 404 or 500 from the extension comes back with its body, not as a tool error. Bodies over 64 KiB are cut and say so. " +
			"Find names and base paths with list_extensions. Claims nothing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in callExtensionIn) (*mcp.CallToolResult, any, error) {
		if !extensionNamePattern.MatchString(in.Name) {
			return errResult(fmt.Errorf("bad extension name %q: lowercase letters, digits and dashes, 1-32 characters", in.Name)), nil, nil
		}
		method := strings.ToUpper(in.Method)
		if method == "" {
			method = http.MethodGet
		}
		if !extensionMethods[method] {
			return errResult(fmt.Errorf("method %q is not one of GET, POST, PUT, DELETE", in.Method)), nil, nil
		}
		p, q, err := extensionPath(in.Path)
		if err != nil {
			return errResult(err), nil, nil
		}
		var body io.Reader
		ct := ""
		if in.Body != "" {
			body = strings.NewReader(in.Body)
			ct = "text/plain; charset=utf-8"
			if json.Valid([]byte(in.Body)) {
				ct = "application/json"
			}
		}
		status, h, b, err := c.send(ctx, method, "/api/ext/"+in.Name+"/"+p, q, ct, body)
		if err != nil {
			return errResult(err), nil, nil
		}
		return textResult(formatExtensionAnswer(status, h.Get("Content-Type"), b)), nil, nil
	})
}

func formatExtensionAnswer(status int, contentType string, b []byte) string {
	var out strings.Builder
	fmt.Fprintf(&out, "status: %d %s\ncontent-type: %s\n\n", status, http.StatusText(status), contentType)
	if !utf8.Valid(b) {
		fmt.Fprintf(&out, "[%d bytes of non-text content not shown]", len(b))
		return out.String()
	}
	if len(b) > maxExtensionBody {
		cut := bytes.ToValidUTF8(b[:maxExtensionBody], nil)
		out.Write(cut)
		fmt.Fprintf(&out, "\n[truncated: showing %d of %d bytes]", len(cut), len(b))
		return out.String()
	}
	out.Write(b)
	return out.String()
}
