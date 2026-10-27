package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandoffAddsTargetsInOrderAndKeepsTheToken(t *testing.T) {
	s := &Server{}
	s.SetHandoffURL("https://mac.example.ts.net:18456/?token=abc")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/handoff?target=avd:Pixel&target=sim:X&target=avd:Pixel", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got HandoffResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := "https://mac.example.ts.net:18456/?target=avd%3APixel&target=sim%3AX&token=abc"
	if got.URL != want {
		t.Fatalf("url = %q, want %q", got.URL, want)
	}
}

func TestHandoffWithoutAnAddressIsNotFound(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/handoff?target=a", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestHandoffRefusesMoreThanFourTargets(t *testing.T) {
	s := &Server{}
	s.SetHandoffURL("https://h:1/")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/handoff?target=a&target=b&target=c&target=d&target=e", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
}
