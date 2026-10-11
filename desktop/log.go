package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// The request log behind the review queue. Every answer the local API gives is recorded on
// this computer. Answers the model wasn't sure about wait for a person, and a small share of
// the sure ones are set aside as spot checks, so Zex can tell how often its AUTO answers
// are right. Each review becomes a label for the accuracy check.

type logReview struct {
	At      int64             `json:"at"`
	Labels  map[string]string `json:"labels"`
	Checked int               `json:"checked"`
	Agreed  int               `json:"agreed"`
}

type logItem struct {
	ID        string          `json:"id"`
	At        int64           `json:"at"` // unix milliseconds
	Set       string          `json:"set"`
	Model     string          `json:"model,omitempty"`
	State     json.RawMessage `json:"state"`
	Decisions map[string]any  `json:"decisions,omitempty"`
	Answers   map[string]any  `json:"answers,omitempty"`
	AllAuto   bool            `json:"all_auto"`
	Audit     bool            `json:"audit,omitempty"`
	AuditRate float64         `json:"audit_rate,omitempty"`
	Error     string          `json:"error,omitempty"`
	Review    *logReview      `json:"review,omitempty"`
}

// status says where an item is: waiting for a person, set aside as a spot check, reviewed,
// answered on its own, or failed.
func (it *logItem) status() string {
	switch {
	case it.Review != nil:
		return "reviewed"
	case it.Error != "":
		return "error"
	case !it.AllAuto:
		return "waiting"
	case it.Audit:
		return "spot"
	}
	return "auto"
}

const logKeep = 10000

var (
	logMu     sync.Mutex
	logItems  []*logItem
	logByID   = map[string]*logItem{}
	logLoaded bool
)

func logPath(name string) string { return filepath.Join(filepath.Dir(apiPath), name) }

// loadLog must be called with logMu held.
func loadLog() {
	if logLoaded {
		return
	}
	logLoaded = true
	logItems, logByID = nil, map[string]*logItem{}
	readLines(logPath("requests.jsonl"), func(line []byte) {
		var it logItem
		if json.Unmarshal(line, &it) == nil && it.ID != "" {
			logItems = append(logItems, &it)
			logByID[it.ID] = &it
		}
	})
	readLines(logPath("reviews.jsonl"), func(line []byte) {
		var rv struct {
			ID     string     `json:"id"`
			Review *logReview `json:"review"`
		}
		if json.Unmarshal(line, &rv) == nil {
			if it := logByID[rv.ID]; it != nil {
				it.Review = rv.Review
			}
		}
	})
	if len(logItems) > logKeep {
		compactLog()
	}
}

func readLines(path string, fn func([]byte)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		fn(sc.Bytes())
	}
}

func appendLine(path string, v any) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(v)
	f.Write(append(data, '\n'))
}

// compactLog keeps the newest logKeep items and rewrites both files. Call with logMu held.
func compactLog() {
	if len(logItems) > logKeep {
		for _, it := range logItems[:len(logItems)-logKeep] {
			delete(logByID, it.ID)
		}
		logItems = append([]*logItem(nil), logItems[len(logItems)-logKeep:]...)
	}
	writeAll := func(name string, each func(*logItem) any) {
		path := logPath(name)
		tmp := path + ".tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		w := bufio.NewWriter(f)
		for _, it := range logItems {
			if v := each(it); v != nil {
				data, _ := json.Marshal(v)
				w.Write(append(data, '\n'))
			}
		}
		w.Flush()
		f.Close()
		os.Rename(tmp, path)
	}
	writeAll("requests.jsonl", func(it *logItem) any { c := *it; c.Review = nil; return c })
	writeAll("reviews.jsonl", func(it *logItem) any {
		if it.Review == nil {
			return nil
		}
		return map[string]any{"id": it.ID, "review": it.Review}
	})
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "r_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b)
}

// chance returns true with probability p.
func chance(p float64) bool {
	if p <= 0 {
		return false
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return float64(n.Int64()) < p*1_000_000
}

func recordItem(it *logItem) {
	logMu.Lock()
	defer logMu.Unlock()
	loadLog()
	logItems = append(logItems, it)
	logByID[it.ID] = it
	appendLine(logPath("requests.jsonl"), it)
	if len(logItems) > logKeep+logKeep/5 {
		compactLog()
	}
}

// handleLog serves the review queue to the page: GET /zex/log?set=&status=&offset=&limit=
func handleLog(w http.ResponseWriter, r *http.Request, origins map[string]bool) {
	if !fromPage(w, r, origins) {
		return
	}
	q := r.URL.Query()
	set, status := q.Get("set"), q.Get("status")
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil {
		limit = 50
	}
	logMu.Lock()
	defer logMu.Unlock()
	loadLog()
	counts := map[string]int{"waiting": 0, "spot": 0, "reviewed": 0, "error": 0, "auto": 0, "all": 0}
	var match []*logItem
	for i := len(logItems) - 1; i >= 0; i-- {
		it := logItems[i]
		if set != "" && it.Set != set {
			continue
		}
		st := it.status()
		counts[st]++
		counts["all"]++
		if status == "" || status == "all" || status == st {
			match = append(match, it)
		}
	}
	if offset > len(match) {
		offset = len(match)
	}
	end := len(match)
	if limit >= 0 && offset+limit < end {
		end = offset + limit
	}
	writeJSON(w, 200, map[string]any{"items": match[offset:end], "total": len(match), "counts": counts})
}

// handleReview saves a person's answers for one item: POST /zex/review
func handleReview(w http.ResponseWriter, r *http.Request, origins map[string]bool) {
	if !fromPage(w, r, origins) {
		return
	}
	var in struct {
		ID      string            `json:"id"`
		Labels  map[string]string `json:"labels"`
		Checked int               `json:"checked"`
		Agreed  int               `json:"agreed"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.ID == "" || len(in.Labels) == 0 {
		writeJSON(w, 400, map[string]any{"error": "Zex: send an id and labels"})
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	loadLog()
	it := logByID[in.ID]
	if it == nil {
		writeJSON(w, 404, map[string]any{"error": "Zex: no request with that id"})
		return
	}
	it.Review = &logReview{At: time.Now().UnixMilli(), Labels: in.Labels, Checked: in.Checked, Agreed: in.Agreed}
	appendLine(logPath("reviews.jsonl"), map[string]any{"id": it.ID, "review": it.Review})
	writeJSON(w, 200, map[string]any{"ok": true, "item": it})
}

// handleStats gives Home its numbers: per set and per day, for the last N days.
func handleStats(w http.ResponseWriter, r *http.Request, origins map[string]bool) {
	if !fromPage(w, r, origins) {
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 90 {
		days = 14
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1))
	labels := make([]string, days)
	for i := range labels {
		labels[i] = start.AddDate(0, 0, i).Format("2006-01-02")
	}
	type series struct {
		Requests      []int `json:"requests"`
		Auto          []int `json:"auto"`
		Errors        []int `json:"errors"`
		AuditChecked  []int `json:"audit_checked"`
		AuditAgreed   []int `json:"audit_agreed"`
		ReviewChecked []int `json:"review_checked"`
		ReviewAgreed  []int `json:"review_agreed"`
	}
	sets := map[string]*series{}
	waiting, spot := map[string]int{}, map[string]int{}
	logMu.Lock()
	defer logMu.Unlock()
	loadLog()
	for _, it := range logItems {
		switch it.status() {
		case "waiting":
			waiting[it.Set]++
		case "spot":
			spot[it.Set]++
		}
		t := time.UnixMilli(it.At)
		if t.Before(start) {
			continue
		}
		d := int(t.Sub(start).Hours() / 24)
		if d < 0 || d >= days {
			continue
		}
		s := sets[it.Set]
		if s == nil {
			s = &series{make([]int, days), make([]int, days), make([]int, days), make([]int, days), make([]int, days), make([]int, days), make([]int, days)}
			sets[it.Set] = s
		}
		s.Requests[d]++
		if it.Error != "" {
			s.Errors[d]++
			continue
		}
		if it.AllAuto {
			s.Auto[d]++
		}
		if it.Review != nil {
			if it.AllAuto {
				s.AuditChecked[d] += it.Review.Checked
				s.AuditAgreed[d] += it.Review.Agreed
			} else {
				s.ReviewChecked[d] += it.Review.Checked
				s.ReviewAgreed[d] += it.Review.Agreed
			}
		}
	}
	writeJSON(w, 200, map[string]any{"days": labels, "sets": sets, "waiting": waiting, "spot": spot})
}

// handleLogClear forgets logged requests, for one set or all: POST /zex/log/clear
func handleLogClear(w http.ResponseWriter, r *http.Request, origins map[string]bool) {
	if !fromPage(w, r, origins) {
		return
	}
	var in struct {
		Set string `json:"set"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in)
	logMu.Lock()
	defer logMu.Unlock()
	loadLog()
	kept := logItems[:0]
	for _, it := range logItems {
		if in.Set != "" && it.Set != in.Set {
			kept = append(kept, it)
		} else {
			delete(logByID, it.ID)
		}
	}
	logItems = kept
	compactLog()
	writeJSON(w, 200, map[string]any{"ok": true, "left": len(logItems)})
}
