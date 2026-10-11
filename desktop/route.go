package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// Routes (cascades): a set can send its questions to a cheap model first, ask a stronger one
// only about the questions the first wasn't sure of, and leave the rest for a person. A stage
// that is down is skipped, so the next one covers for it.

type apiStage struct {
	Connection apiConn `json:"connection"`
	Model      string  `json:"model"`
	Cost       float64 `json:"cost,omitempty"` // dollars per 1,000 requests
}

type stageLog struct {
	Name  string  `json:"name"`
	Model string  `json:"model"`
	Asked int     `json:"asked"` // questions sent to this stage
	Auto  int     `json:"auto"`  // of those, answered with enough confidence
	Ms    int64   `json:"ms"`
	Cost  float64 `json:"cost,omitempty"`
	Error string  `json:"error,omitempty"`
}

func (s stageLog) label() string { return s.Name + " · " + s.Model }

type routeResult struct {
	Answers   map[string]any
	Decisions map[string]any
	AllAuto   bool
	Model     string
	Stages    []stageLog
	By        int     // the stage that finished the request, or -1 when it needs a person
	Cost      float64 // what this request cost
	Baseline  float64 // what it would have cost on the last, strongest stage alone
	Ms        int64
	Err       error
}

func runRoute(cfg apiConfig, set *apiSet, model string, state json.RawMessage) routeResult {
	route := set.Route
	if len(route) == 0 {
		route = []apiStage{{Connection: *cfg.Connection, Model: model}}
	}
	var qs map[string]json.RawMessage
	json.Unmarshal(set.Questions, &qs)
	keys := make([]string, 0, len(qs))
	for k := range qs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	res := routeResult{Answers: map[string]any{}, Decisions: map[string]any{}, By: -1, Baseline: route[len(route)-1].Cost / 1000}
	remaining, start := keys, time.Now()
	var lastErr error
	for i, st := range route {
		sub := map[string]json.RawMessage{}
		for _, k := range remaining {
			sub[k] = qs[k]
		}
		subJSON, _ := json.Marshal(sub)
		t0 := time.Now()
		data, err := callModel(st.Connection, st.Model, state, subJSON)
		sl := stageLog{Name: st.Connection.Name, Model: st.Model, Asked: len(remaining), Ms: time.Since(t0).Milliseconds()}
		if sl.Name == "" {
			sl.Name = st.Connection.URL
		}
		if err != nil {
			sl.Error, lastErr = err.Error(), err
			res.Stages = append(res.Stages, sl)
			continue
		}
		sl.Cost = st.Cost / 1000
		res.Cost += sl.Cost
		answers, _ := data["answers"].(map[string]any)
		dec, _ := decide(answers, set.Thresholds, cfg.Thr)
		res.Model = st.Model
		if m, ok := data["model"].(string); ok && m != "" {
			res.Model = m
		}
		var next []string
		for _, k := range remaining {
			a, ok := answers[k]
			if !ok {
				next = append(next, k)
				continue
			}
			d, _ := dec[k].(map[string]any)
			if len(route) > 1 {
				d["by"] = sl.label()
			}
			res.Answers[k], res.Decisions[k] = a, d
			if d["verdict"] == "auto" {
				sl.Auto++
			} else {
				next = append(next, k)
			}
		}
		res.Stages = append(res.Stages, sl)
		remaining = next
		if len(remaining) == 0 {
			res.By = i
			break
		}
	}
	res.Ms = time.Since(start).Milliseconds()
	res.AllAuto = len(remaining) == 0 && len(keys) > 0
	if len(res.Answers) == 0 && lastErr != nil {
		res.Err = lastErr
	}
	return res
}

// handlePoints gives the Home atlas one small row per recent request:
// [time, set, weakest confidence, status, stage that finished it, milliseconds].
// Status: 0 auto, 1 waiting, 2 spot check, 3 reviewed and right, 4 reviewed and corrected, 5 error.
func handlePoints(w http.ResponseWriter, r *http.Request, origins map[string]bool) {
	if !fromPage(w, r, origins) {
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 90 {
		days = 7
	}
	since := time.Now().AddDate(0, 0, -days).UnixMilli()
	logMu.Lock()
	defer logMu.Unlock()
	loadLog()
	sets, index, points := []string{}, map[string]int{}, [][]any{}
	for i := len(logItems) - 1; i >= 0 && len(points) < 4000; i-- {
		it := logItems[i]
		if it.At < since {
			break
		}
		si, ok := index[it.Set]
		if !ok {
			si = len(sets)
			index[it.Set] = si
			sets = append(sets, it.Set)
		}
		weakest := 1.0
		for _, d := range it.Decisions {
			if m, ok := d.(map[string]any); ok {
				if c, ok := m["confidence"].(float64); ok && c < weakest {
					weakest = c
				}
			}
		}
		code := map[string]int{"auto": 0, "waiting": 1, "spot": 2, "reviewed": 3, "error": 5}[it.status()]
		if it.Review != nil && it.Review.Agreed < it.Review.Checked {
			code = 4
		}
		if it.Error != "" {
			weakest = 0
		}
		by := it.By
		if !it.AllAuto {
			by = -1
		}
		points = append(points, []any{it.At, si, weakest, code, by, it.Ms})
	}
	writeJSON(w, 200, map[string]any{"sets": sets, "points": points})
}
