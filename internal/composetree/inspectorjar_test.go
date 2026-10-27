package composetree

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

func aar(t *testing.T, jar []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for name, body := range map[string][]byte{"classes.jar": []byte("not this one"), "inspector.jar": jar} {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(body)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestInspectorJarRejectsABadVersion(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	if _, err := InspectorJar(context.Background(), t.TempDir(), srv.URL, "1.12.1; rm -rf /"); err == nil {
		t.Fatal("want an error for a version with shell in it")
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests, want none before the version is checked", hits.Load())
	}
}

func TestInspectorJarCaches(t *testing.T) {
	body := aar(t, []byte("the inspector"))
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Write(body)
	}))
	defer srv.Close()
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		p, err := InspectorJar(context.Background(), dir, srv.URL, "1.12.1")
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "the inspector" {
			t.Fatalf("jar = %q", got)
		}
	}
	if len(paths) != 1 || paths[0] != "/1.12.1/ui-android-1.12.1.aar" {
		t.Fatalf("requests = %v, want one for /1.12.1/ui-android-1.12.1.aar", paths)
	}
}

func TestInspectorJarReportsAMissingVersion(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := InspectorJar(context.Background(), t.TempDir(), srv.URL, "9.9.9"); err == nil {
		t.Fatal("want an error for a 404")
	}
}
