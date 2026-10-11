package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// The local API: while Zex runs, each saved question set is an endpoint on 127.0.0.1.
// The page sends the sets and the active connection here (see /zex/api-config); they are
// kept in a config file that only this user can read, and only while the API is on.

type apiSet struct {
	Slug       string             `json:"slug"`
	Name       string             `json:"name"`
	Questions  json.RawMessage    `json:"questions"`
	Thresholds map[string]float64 `json:"thresholds,omitempty"`
}

type apiConn struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	URL  string `json:"url"`
	Key  string `json:"key,omitempty"`
}

type apiConfig struct {
	Enabled    bool     `json:"enabled"`
	Token      string   `json:"token,omitempty"`
	Model      string   `json:"model,omitempty"`
	Thr        float64  `json:"thr,omitempty"`
	Connection *apiConn `json:"connection,omitempty"`
	Sets       []apiSet `json:"sets,omitempty"`
}

var (
	apiMu     sync.Mutex
	apiCfg    apiConfig
	apiLoaded bool
	apiPath   = defaultAPIPath()
)

func defaultAPIPath() string {
	if d := os.Getenv("ZEX_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "api.json")
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "Zex", "api.json")
}

// loadAPIConfig must be called with apiMu held.
func loadAPIConfig() {
	if apiLoaded {
		return
	}
	apiLoaded = true
	if data, err := os.ReadFile(apiPath); err == nil {
		json.Unmarshal(data, &apiCfg)
	}
}

func saveAPIConfig() error {
	if apiPath == "" {
		return errors.New("no config folder on this system")
	}
	if err := os.MkdirAll(filepath.Dir(apiPath), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(apiCfg, "", "  ")
	tmp := apiPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, apiPath)
}

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "zex_" + hex.EncodeToString(b)
}

// handleAPIConfig is how the page turns the API on or off and keeps it current. It has the
// same guards as the relay: a custom header (so other sites can't send it without a CORS
// preflight, which this server never grants) and, when present, this page's origin.
func handleAPIConfig(w http.ResponseWriter, r *http.Request, origins map[string]bool) {
	if r.Header.Get("X-Zex") != "1" {
		writeJSON(w, 403, map[string]any{"error": "Zex: missing X-Zex header"})
		return
	}
	if o := r.Header.Get("Origin"); o != "" && !origins[o] {
		writeJSON(w, 403, map[string]any{"error": "Zex: origin " + o + " not allowed"})
		return
	}
	apiMu.Lock()
	defer apiMu.Unlock()
	loadAPIConfig()
	if r.Method == http.MethodPost {
		var in struct {
			apiConfig
			Regenerate bool `json:"regenerate"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]any{"error": "Zex: bad JSON"})
			return
		}
		apiCfg.Enabled = in.Enabled
		if apiCfg.Token == "" || in.Regenerate {
			apiCfg.Token = newToken()
		}
		if in.Enabled {
			apiCfg.Model, apiCfg.Thr, apiCfg.Connection, apiCfg.Sets = in.Model, in.Thr, in.Connection, in.Sets
		} else {
			apiCfg.Model, apiCfg.Thr, apiCfg.Connection, apiCfg.Sets = "", 0, nil, nil
		}
		if err := saveAPIConfig(); err != nil {
			writeJSON(w, 500, map[string]any{"error": "Zex could not save its API settings: " + err.Error()})
			return
		}
	}
	writeJSON(w, 200, map[string]any{"enabled": apiCfg.Enabled, "token": apiCfg.Token})
}

func handleAPI(w http.ResponseWriter, r *http.Request) {
	apiMu.Lock()
	loadAPIConfig()
	cfg := apiCfg
	apiMu.Unlock()
	if !cfg.Enabled {
		writeJSON(w, 403, map[string]any{"error": "The Zex local API is off. Turn it on in Zex, under Local API."})
		return
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		got = r.Header.Get("X-Zex-Token")
	}
	if cfg.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(cfg.Token)) != 1 {
		writeJSON(w, 401, map[string]any{"error": "Missing or wrong token. Send Authorization: Bearer <token>, from Zex under Local API."})
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api"), "/")
	switch {
	case path == "" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"ok": true, "app": "zex", "version": version, "sets": len(cfg.Sets)})
	case path == "sets" && r.Method == http.MethodGet:
		out := []map[string]any{}
		for _, s := range cfg.Sets {
			var q map[string]map[string]any
			json.Unmarshal(s.Questions, &q)
			types := map[string]any{}
			for k, v := range q {
				types[k] = v["type"]
			}
			out = append(out, map[string]any{"slug": s.Slug, "name": s.Name, "questions": types, "endpoint": "/api/sets/" + s.Slug})
		}
		writeJSON(w, 200, map[string]any{"sets": out})
	case strings.HasPrefix(path, "sets/") && r.Method == http.MethodPost:
		slug, _ := url.PathUnescape(strings.TrimPrefix(path, "sets/"))
		runSet(w, r, cfg, slug)
	default:
		writeJSON(w, 404, map[string]any{"error": "Not found. POST /api/sets/<set> with {\"text\": \"...\"}, or GET /api/sets."})
	}
}

func runSet(w http.ResponseWriter, r *http.Request, cfg apiConfig, slug string) {
	var set *apiSet
	for i := range cfg.Sets {
		if cfg.Sets[i].Slug == slug {
			set = &cfg.Sets[i]
		}
	}
	if set == nil {
		writeJSON(w, 404, map[string]any{"error": fmt.Sprintf("No set called %q. GET /api/sets lists them.", slug)})
		return
	}
	if cfg.Connection == nil || cfg.Connection.URL == "" {
		writeJSON(w, 503, map[string]any{"error": "Zex has no connection to answer with. Open Zex and pick one in Connections."})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var in struct {
		Text  *string           `json:"text"`
		State json.RawMessage   `json:"state"`
		Items []json.RawMessage `json:"items"`
		Model string            `json:"model"`
	}
	trimmed := bytes.TrimSpace(body)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || (len(trimmed) > 0 && trimmed[0] == '{') {
		if err := json.Unmarshal(body, &in); err != nil {
			writeJSON(w, 400, map[string]any{"error": "The body isn't valid JSON. Send {\"text\": \"...\"}."})
			return
		}
	} else if len(trimmed) > 0 {
		t := string(body)
		in.Text = &t
	}
	model := in.Model
	if model == "" {
		model = cfg.Model
	}
	one := func(state json.RawMessage) map[string]any {
		data, err := callModel(*cfg.Connection, model, state, set.Questions)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		answers, _ := data["answers"].(map[string]any)
		dec, allAuto := decide(answers, set.Thresholds, cfg.Thr)
		m := data["model"]
		if m == nil {
			m = model
		}
		return map[string]any{"set": set.Slug, "model": m, "decisions": dec, "all_auto": allAuto, "answers": answers}
	}
	switch {
	case len(in.Items) > 0:
		if len(in.Items) > 100 {
			writeJSON(w, 400, map[string]any{"error": "Send at most 100 items at once."})
			return
		}
		results := make([]map[string]any, 0, len(in.Items))
		for _, it := range in.Items {
			res := one(it)
			res["state"] = it
			results = append(results, res)
		}
		writeJSON(w, 200, map[string]any{"set": set.Slug, "results": results})
	case in.Text != nil || len(in.State) > 0:
		state := in.State
		if in.Text != nil {
			state, _ = json.Marshal(*in.Text)
		}
		res := one(state)
		if e, ok := res["error"]; ok {
			writeJSON(w, 502, map[string]any{"error": e})
			return
		}
		writeJSON(w, 200, res)
	default:
		writeJSON(w, 400, map[string]any{"error": "Send {\"text\": \"...\"}, {\"state\": ...} or {\"items\": [...]}."})
	}
}

// callModel sends one request to the connection, the same way the page does: Ollaya first
// tries its own /api/decide, everything else gets POST /v1/systemone.
func callModel(c apiConn, model string, state, questions json.RawMessage) (map[string]any, error) {
	payload, _ := json.Marshal(map[string]any{"model": model, "state": state, "questions": questions})
	base := strings.TrimRight(c.URL, "/")
	paths := []string{"/v1/systemone"}
	if c.Kind == "ollaya" {
		paths = []string{"/api/decide", "/v1/systemone"}
	}
	name := c.Name
	if name == "" {
		name = base
	}
	for i, p := range paths {
		req, err := http.NewRequest(http.MethodPost, base+p, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("the connection URL %q isn't valid", c.URL)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Zex/"+version)
		if c.Key != "" {
			req.Header.Set("Authorization", "Bearer "+c.Key)
		}
		client := remote
		if loopback[req.URL.Hostname()] {
			client = direct
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Zex could not reach %s: %v", name, err)
		}
		data, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
		res.Body.Close()
		if (res.StatusCode == 404 || res.StatusCode == 405) && i < len(paths)-1 {
			continue
		}
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("%s sent something that isn't JSON (HTTP %d)", name, res.StatusCode)
		}
		if res.StatusCode >= 300 || out["error"] != nil || out["error_type"] != nil {
			msg := out["error"]
			if msg == nil {
				msg = out["message"]
			}
			if msg == nil {
				msg = out["detail"]
			}
			return nil, fmt.Errorf("%s answered with an error (HTTP %d): %v", name, res.StatusCode, msg)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s has no decision endpoint", name)
}

// decide turns raw answers into verdicts with the same rules as the page: the top option and
// its probability, AUTO when it reaches the question's threshold (or the global one).
func decide(answers map[string]any, thresholds map[string]float64, global float64) (map[string]any, bool) {
	if global <= 0 {
		global = 0.8
	}
	out, all := map[string]any{}, len(answers) > 0
	for k, v := range answers {
		a, ok := v.(map[string]any)
		if !ok {
			continue
		}
		label, p := topAnswer(a)
		t := global
		if x, ok := thresholds[k]; ok && x > 0 {
			t = x
		}
		verdict := "review"
		if p >= t {
			verdict = "auto"
		} else {
			all = false
		}
		out[k] = map[string]any{"answer": label, "confidence": math.Round(p*10000) / 10000, "verdict": verdict, "threshold": t}
	}
	return out, all
}

func topAnswer(a map[string]any) (string, float64) {
	num := func(v any) float64 { f, _ := v.(float64); return f }
	probs, _ := a["probabilities"].(map[string]any)
	switch a["type"] {
	case "noul":
		p := num(a["noul"])
		if p >= 0.5 {
			return "true", p
		}
		return "false", 1 - p
	case "choice":
		label := fmt.Sprint(a["choice"])
		if p, ok := probs[label]; ok {
			return label, num(p)
		}
		return label, num(a["confidence"])
	case "score":
		keys := make([]string, 0, len(probs))
		for k := range probs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		best := ""
		for _, k := range keys {
			if best == "" || num(probs[k]) > num(probs[best]) {
				best = k
			}
		}
		if best == "" {
			return "", 0
		}
		label := best
		if legend, ok := a["legend"].(map[string]any); ok && legend[best] != nil {
			label = fmt.Sprint(legend[best])
		}
		return label, num(probs[best])
	}
	return "", 0
}
