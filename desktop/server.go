package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The same relay as server.py: forwards a request to a decision-model API path, only when
// it comes from the Zex page, so cloud servers that send no CORS headers still work.
var (
	// decisions and model lists, and on Ollaya the model store: details, downloads and removal
	allowedPath = regexp.MustCompile(`/(v1/(systemone|decisions|models)|api/(decide|tags|ps|show|version|pull|delete))/?$`)
	loopback    = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	// Local model servers never go through a system proxy; remote ones may need it.
	direct = &http.Client{Timeout: 180 * time.Second, Transport: &http.Transport{Proxy: nil}}
	remote = &http.Client{Timeout: 180 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	// A download can take many minutes, so only its first answer has a deadline.
	directLong = &http.Client{Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 180 * time.Second}}
	remoteLong = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 180 * time.Second}}
	// Ollaya's catalog of models to download; the page reads it through /zex/catalog.
	catalogURL = "https://ollaya.dev/search.json"
	catalog    struct {
		sync.Mutex
		at   time.Time
		data []byte
	}
)

func newHandler(site fs.FS, port int) http.Handler {
	origins := map[string]bool{
		fmt.Sprintf("http://127.0.0.1:%d", port): true,
		fmt.Sprintf("http://localhost:%d", port): true,
	}
	files := http.FileServer(http.FS(site))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		switch {
		case strings.HasPrefix(r.URL.Path, "/zex/health"):
			writeJSON(w, 200, map[string]any{"ok": true, "app": "zex", "relay": true})
		case strings.HasPrefix(r.URL.Path, "/zex/proxy"):
			relay(w, r, origins)
		case strings.HasPrefix(r.URL.Path, "/zex/api-config"):
			handleAPIConfig(w, r, origins)
		case r.URL.Path == "/zex/log/clear" && r.Method == http.MethodPost:
			handleLogClear(w, r, origins)
		case r.URL.Path == "/zex/log":
			handleLog(w, r, origins)
		case r.URL.Path == "/zex/review" && r.Method == http.MethodPost:
			handleReview(w, r, origins)
		case r.URL.Path == "/zex/stats":
			handleStats(w, r, origins)
		case r.URL.Path == "/zex/points":
			handlePoints(w, r, origins)
		case r.URL.Path == "/zex/catalog":
			handleCatalog(w, r, origins)
		case r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/"):
			handleAPI(w, r)
		case r.Method == http.MethodGet || r.Method == http.MethodHead:
			files.ServeHTTP(w, r)
		default:
			writeJSON(w, 405, map[string]any{"error": "method not allowed"})
		}
	})
}

func relay(w http.ResponseWriter, r *http.Request, origins map[string]bool) {
	if r.Header.Get("X-Zex") != "1" {
		writeJSON(w, 403, map[string]any{"error": "Zex relay: missing X-Zex header"})
		return
	}
	if o := r.Header.Get("Origin"); o != "" && !origins[o] {
		writeJSON(w, 403, map[string]any{"error": "Zex relay: origin " + o + " not allowed"})
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
		return
	}
	target := r.URL.Query().Get("u")
	t, err := url.Parse(target)
	if err != nil || (t.Scheme != "http" && t.Scheme != "https") || t.Host == "" || !allowedPath.MatchString(t.Path) {
		writeJSON(w, 400, map[string]any{"error": "Zex relay: only decision-model API paths can be relayed", "target": target})
		return
	}
	var body io.Reader
	if r.Method != http.MethodGet {
		body = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": "Zex relay: " + err.Error()})
		return
	}
	ctype := r.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "application/json"
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Zex/"+version)
	if key := r.Header.Get("X-Zex-Key"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	long := strings.HasSuffix(strings.TrimRight(t.Path, "/"), "/api/pull")
	client := remote
	switch {
	case loopback[t.Hostname()] && long:
		client = directLong
	case loopback[t.Hostname()]:
		client = direct
	case long:
		client = remoteLong
	}
	res, err := client.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": fmt.Sprintf("Zex relay could not reach %s: %v", t.Host, err), "code": "UNREACHABLE"})
		return
	}
	defer res.Body.Close()
	rt := res.Header.Get("Content-Type")
	if rt == "" {
		rt = "application/json"
	}
	w.Header().Set("Content-Type", rt)
	w.WriteHeader(res.StatusCode)
	// pass each piece on as it arrives, so a download's progress shows while it runs
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// handleCatalog serves Ollaya's model catalog, fetched at most once an hour. The page can't
// fetch it itself: ollaya.dev sends no CORS headers.
func handleCatalog(w http.ResponseWriter, r *http.Request, origins map[string]bool) {
	if !fromPage(w, r, origins) {
		return
	}
	catalog.Lock()
	defer catalog.Unlock()
	if catalog.data == nil || time.Since(catalog.at) > time.Hour {
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, catalogURL, nil)
		req.Header.Set("User-Agent", "Zex/"+version)
		res, err := remote.Do(req)
		if err == nil {
			data, rerr := io.ReadAll(io.LimitReader(res.Body, 8<<20))
			res.Body.Close()
			switch {
			case rerr != nil:
				err = rerr
			case res.StatusCode != 200 || !json.Valid(data):
				err = fmt.Errorf("HTTP %d", res.StatusCode)
			default:
				catalog.data, catalog.at = data, time.Now()
			}
		}
		if catalog.data == nil {
			writeJSON(w, 502, map[string]any{"error": fmt.Sprintf("Zex couldn't load Ollaya's model catalog: %v", err), "code": "UNREACHABLE"})
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(catalog.data)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
}

// listen binds 127.0.0.1 on the preferred port, so the page keeps one origin (and its saved
// history and connections) between launches. If another Zex already holds the port, it
// returns that one's URL instead of a listener.
func listen(port int) (net.Listener, int, string) {
	for p := port; p < port+20; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			return ln, p, ""
		}
		if isZex(p) {
			return nil, p, fmt.Sprintf("http://127.0.0.1:%d/", p)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, ""
}

func isZex(port int) bool {
	c := http.Client{Timeout: 800 * time.Millisecond}
	res, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/zex/health", port))
	if err != nil {
		return false
	}
	defer res.Body.Close()
	var h struct{ App string }
	return json.NewDecoder(res.Body).Decode(&h) == nil && h.App == "zex"
}
