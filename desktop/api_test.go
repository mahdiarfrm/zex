package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zex/web"
)

// fakeModel answers like a decision server: one choice, one noul and one score question.
func fakeModel(t *testing.T, gotPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.URL.Path
		if r.URL.Path == "/api/decide" {
			w.WriteHeader(404)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["state"] == nil || body["questions"] == nil {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"missing state or questions"}`)
			return
		}
		io.WriteString(w, `{"model":"laya:en","answers":{
			"team":{"type":"choice","choice":"billing","probabilities":{"billing":0.92,"technical":0.08},"confidence":0.7},
			"refund":{"type":"noul","noul":0.85},
			"mood":{"type":"score","probabilities":{"0":0.1,"1":0.6,"2":0.3},"legend":{"0":"calm","1":"annoyed","2":"angry"}}}}`)
	}))
}

func setup(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	apiMu.Lock()
	apiPath, apiCfg, apiLoaded = filepath.Join(dir, "api.json"), apiConfig{}, false
	apiMu.Unlock()
	return newHandler(web.FS, 18765)
}

func configure(t *testing.T, h http.Handler, body string) map[string]any {
	t.Helper()
	r := get(t, h, "POST", "/zex/api-config", map[string]string{"X-Zex": "1", "Origin": "http://127.0.0.1:18765", "Content-Type": "application/json"}, body)
	if r.Code != 200 {
		t.Fatalf("api-config: %d %s", r.Code, r.Body)
	}
	var out map[string]any
	json.Unmarshal(r.Body.Bytes(), &out)
	return out
}

func TestAPIConfigGuards(t *testing.T) {
	h := setup(t)
	if r := get(t, h, "POST", "/zex/api-config", nil, `{"enabled":true}`); r.Code != 403 {
		t.Fatalf("missing X-Zex should be 403, got %d", r.Code)
	}
	if r := get(t, h, "POST", "/zex/api-config", map[string]string{"X-Zex": "1", "Origin": "https://evil.example"}, `{"enabled":true}`); r.Code != 403 {
		t.Fatalf("foreign origin should be 403, got %d", r.Code)
	}
	if r := get(t, h, "GET", "/api/sets", nil, ""); r.Code != 403 {
		t.Fatalf("API off should be 403, got %d", r.Code)
	}
}

func TestAPIRunsASet(t *testing.T) {
	var path string
	up := fakeModel(t, &path)
	defer up.Close()
	h := setup(t)
	cfg := configure(t, h, `{"enabled":true,"model":"laya:latest","thr":0.8,
		"connection":{"kind":"ollaya","name":"Ollaya","url":"`+up.URL+`"},
		"sets":[{"slug":"triage","name":"Triage","questions":{"team":{"type":"choice"},"refund":{"type":"noul"},"mood":{"type":"score"}},"thresholds":{"refund":0.9}}]}`)
	token, _ := cfg["token"].(string)
	if !strings.HasPrefix(token, "zex_") || cfg["enabled"] != true {
		t.Fatalf("config: %v", cfg)
	}
	auth := map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"}

	if r := get(t, h, "POST", "/api/sets/triage", map[string]string{"Authorization": "Bearer nope"}, `{"text":"hi"}`); r.Code != 401 {
		t.Fatalf("wrong token should be 401, got %d", r.Code)
	}
	if r := get(t, h, "POST", "/api/sets/missing", auth, `{"text":"hi"}`); r.Code != 404 {
		t.Fatalf("unknown set should be 404, got %d", r.Code)
	}
	r := get(t, h, "POST", "/api/sets/triage", auth, `{"text":"I was charged twice"}`)
	if r.Code != 200 {
		t.Fatalf("run: %d %s", r.Code, r.Body)
	}
	if path != "/v1/systemone" {
		t.Fatalf("Ollaya should fall back to /v1/systemone, last path was %s", path)
	}
	var out struct {
		Model     string                    `json:"model"`
		AllAuto   bool                      `json:"all_auto"`
		Decisions map[string]map[string]any `json:"decisions"`
	}
	json.Unmarshal(r.Body.Bytes(), &out)
	if out.Model != "laya:en" || out.AllAuto {
		t.Fatalf("model %q all_auto %v", out.Model, out.AllAuto)
	}
	want := map[string][3]any{
		"team":   {"billing", 0.92, "auto"},
		"refund": {"true", 0.85, "review"}, // the set's own threshold, 0.9
		"mood":   {"annoyed", 0.6, "review"},
	}
	for k, w := range want {
		d := out.Decisions[k]
		if d["answer"] != w[0] || d["confidence"] != w[1] || d["verdict"] != w[2] {
			t.Errorf("%s: got %v, want %v", k, d, w)
		}
	}

	// plain text bodies and batches of items
	if r := get(t, h, "POST", "/api/sets/triage", map[string]string{"Authorization": "Bearer " + token, "Content-Type": "text/plain"}, "refund please"); r.Code != 200 {
		t.Fatalf("plain text: %d %s", r.Code, r.Body)
	}
	r = get(t, h, "POST", "/api/sets/triage", auth, `{"items":["one","two"]}`)
	var batch struct{ Results []map[string]any }
	json.Unmarshal(r.Body.Bytes(), &batch)
	if r.Code != 200 || len(batch.Results) != 2 {
		t.Fatalf("items: %d %s", r.Code, r.Body)
	}
	if r := get(t, h, "GET", "/api/sets", map[string]string{"X-Zex-Token": token}, ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"triage"`) {
		t.Fatalf("list: %d %s", r.Code, r.Body)
	}

	// the config file is private, and turning the API off forgets the connection and its key
	if fi, err := os.Stat(apiPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config file mode: %v %v", fi, err)
	}
	cfg = configure(t, h, `{"enabled":false}`)
	if cfg["token"] != token {
		t.Fatalf("turning off should keep the token")
	}
	data, _ := os.ReadFile(apiPath)
	if strings.Contains(string(data), up.URL) || strings.Contains(string(data), "triage") {
		t.Fatalf("off should drop sets and connection, file has: %s", data)
	}
	if r := get(t, h, "POST", "/api/sets/triage", auth, `{"text":"hi"}`); r.Code != 403 {
		t.Fatalf("off should be 403, got %d", r.Code)
	}
	cfg = configure(t, h, `{"enabled":true,"regenerate":true}`)
	if cfg["token"] == token {
		t.Fatalf("regenerate should make a new token")
	}
}
