package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"zex/web"
)

func get(t *testing.T, h http.Handler, method, path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthAndPage(t *testing.T) {
	h := newHandler(web.FS, 18765)
	if r := get(t, h, "GET", "/zex/health", nil, ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"app":"zex"`) {
		t.Fatalf("health: %d %s", r.Code, r.Body)
	}
	if r := get(t, h, "GET", "/", nil, ""); r.Code != 200 || !strings.Contains(r.Body.String(), "<title>") {
		t.Fatalf("index: %d", r.Code)
	}
	if r := get(t, h, "GET", "/fonts/vt323-latin-400.woff2", nil, ""); r.Code != 200 {
		t.Fatalf("font: %d", r.Code)
	}
	if r := get(t, h, "GET", "/embed.go", nil, ""); r.Code != 404 {
		t.Fatalf("source file should not be served: %d", r.Code)
	}
}

func TestRelayGuards(t *testing.T) {
	h := newHandler(web.FS, 18765)
	ok := "/zex/proxy?u=" + url.QueryEscape("http://127.0.0.1:1/v1/models")
	if r := get(t, h, "GET", ok, nil, ""); r.Code != 403 {
		t.Fatalf("missing X-Zex should be 403, got %d", r.Code)
	}
	if r := get(t, h, "GET", ok, map[string]string{"X-Zex": "1", "Origin": "https://evil.example"}, ""); r.Code != 403 {
		t.Fatalf("foreign origin should be 403, got %d", r.Code)
	}
	bad := "/zex/proxy?u=" + url.QueryEscape("http://127.0.0.1:1/etc/passwd")
	if r := get(t, h, "GET", bad, map[string]string{"X-Zex": "1"}, ""); r.Code != 400 {
		t.Fatalf("non-API path should be 400, got %d", r.Code)
	}
	file := "/zex/proxy?u=" + url.QueryEscape("file:///v1/models")
	if r := get(t, h, "GET", file, map[string]string{"X-Zex": "1"}, ""); r.Code != 400 {
		t.Fatalf("file URL should be 400, got %d", r.Code)
	}
	if r := get(t, h, "GET", ok, map[string]string{"X-Zex": "1", "Origin": "http://127.0.0.1:18765"}, ""); r.Code != 502 || !strings.Contains(r.Body.String(), "UNREACHABLE") {
		t.Fatalf("unreachable upstream should be 502 UNREACHABLE, got %d %s", r.Code, r.Body)
	}
}

func TestRelayForwards(t *testing.T) {
	var gotAuth, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		io.WriteString(w, `{"answers":{}}`)
	}))
	defer up.Close()
	h := newHandler(web.FS, 18765)
	path := "/zex/proxy?u=" + url.QueryEscape(up.URL+"/v1/systemone")
	r := get(t, h, "POST", path, map[string]string{"X-Zex": "1", "X-Zex-Key": "k123", "Content-Type": "application/json"}, `{"state":"hi"}`)
	if r.Code != 201 || r.Body.String() != `{"answers":{}}` {
		t.Fatalf("relay response: %d %s", r.Code, r.Body)
	}
	if gotAuth != "Bearer k123" || gotBody != `{"state":"hi"}` {
		t.Fatalf("upstream saw auth=%q body=%q", gotAuth, gotBody)
	}
}

func TestSaveNameIsSanitized(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	revealFile = func(string) {}
	p, err := saveDownload("../../etc/zex batch.csv", "a,b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p, "/Downloads/zex batch.csv") {
		t.Fatalf("saved to %s", p)
	}
}
