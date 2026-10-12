package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Results sent back. A set can have a webhook: Zex posts each final answer to it, right away
// when the model was sure, and after a person reviews it otherwise. A delivery that fails is
// tried again later, so a receiver that is down for a while still gets every answer.

type delivery struct {
	ID    string          `json:"id"`
	Event string          `json:"event"`
	Item  string          `json:"item,omitempty"`
	Set   string          `json:"set"`
	URL   string          `json:"url"`
	Body  json.RawMessage `json:"body"`
	At    int64           `json:"at"`
	Tries int             `json:"tries"`
	Next  int64           `json:"next,omitempty"` // when the next try is due, while pending
	State string          `json:"state"`          // pending, sent or failed
	Code  int             `json:"code,omitempty"`
	Error string          `json:"error,omitempty"`
	Last  int64           `json:"last,omitempty"`
	busy  bool
}

const hookKeep = 300

var (
	hookMu     sync.Mutex
	hookList   []*delivery
	hookLoaded bool
	hookWake   = make(chan struct{}, 1)
	hookOnce   sync.Once
	// waits between tries: seven tries over about nine hours
	hookDelays  = []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}
	hookTimeout = 15 * time.Second
)

func validHookURL(u string) bool {
	t, err := url.Parse(u)
	return err == nil && (t.Scheme == "http" || t.Scheme == "https") && t.Host != ""
}

// loadHooks must be called with hookMu held.
func loadHooks() {
	if hookLoaded {
		return
	}
	hookLoaded = true
	if data, err := os.ReadFile(logPath("deliveries.json")); err == nil {
		json.Unmarshal(data, &hookList)
	}
}

// saveHooks must be called with hookMu held.
func saveHooks() {
	if len(hookList) > hookKeep {
		hookList = append([]*delivery(nil), hookList[len(hookList)-hookKeep:]...)
	}
	path := logPath("deliveries.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	data, _ := json.Marshal(hookList)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, path)
	}
}

func startHooks() { hookOnce.Do(func() { go hookLoop() }) }

// queueHook adds a delivery for the sender. State is "pending" for the sender to send, or
// "sending" when the caller sends it itself.
func queueHook(set apiSet, event, item string, payload map[string]any, state string) *delivery {
	d := &delivery{ID: "d_" + strings.TrimPrefix(newID(), "r_"), Event: event, Item: item, Set: set.Slug, URL: set.Webhook, At: time.Now().UnixMilli(), State: state}
	payload["event"], payload["delivery"] = event, d.ID
	d.Body, _ = json.Marshal(payload)
	hookMu.Lock()
	loadHooks()
	hookList = append(hookList, d)
	saveHooks()
	hookMu.Unlock()
	startHooks()
	select {
	case hookWake <- struct{}{}:
	default:
	}
	return d
}

func hookLoop() {
	for {
		now := time.Now().UnixMilli()
		wait := 30 * time.Second
		var due []*delivery
		hookMu.Lock()
		loadHooks()
		for _, d := range hookList {
			if d.State != "pending" || d.busy {
				continue
			}
			if d.Next <= now {
				due = append(due, d)
			} else if w := time.Duration(d.Next-now) * time.Millisecond; w < wait {
				wait = w
			}
		}
		hookMu.Unlock()
		for _, d := range due {
			sendHook(d, true)
		}
		if len(due) > 0 {
			continue
		}
		select {
		case <-hookWake:
		case <-time.After(wait):
		}
	}
}

// sign gives the X-Zex-Signature header: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">.
func sign(secret string, t int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(m, "%d.", t)
	m.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", t, hex.EncodeToString(m.Sum(nil)))
}

// sendHook makes one try. With retry, a failure that may pass is tried again later.
func sendHook(d *delivery, retry bool) {
	hookMu.Lock()
	if d.busy {
		hookMu.Unlock()
		return
	}
	d.busy = true
	target, body, event, id := d.URL, d.Body, d.Event, d.ID
	hookMu.Unlock()

	apiMu.Lock()
	loadAPIConfig()
	secret := apiCfg.HookSecret
	apiMu.Unlock()

	code, errText := 0, ""
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		errText = "the webhook URL isn't valid"
	} else {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Zex/"+version)
		req.Header.Set("X-Zex-Event", event)
		req.Header.Set("X-Zex-Delivery", id)
		req.Header.Set("X-Zex-Signature", sign(secret, time.Now().Unix(), body))
		tr := remote.Transport
		if loopback[req.URL.Hostname()] {
			tr = direct.Transport
		}
		res, err := (&http.Client{Timeout: hookTimeout, Transport: tr}).Do(req)
		if err != nil {
			errText = err.Error()
		} else {
			io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
			res.Body.Close()
			code = res.StatusCode
		}
	}

	hookMu.Lock()
	defer hookMu.Unlock()
	d.busy = false
	d.Tries++
	d.Code, d.Error, d.Last, d.Next = code, errText, time.Now().UnixMilli(), 0
	switch {
	case code >= 200 && code < 300:
		d.State, d.Error = "sent", ""
	case code >= 400 && code < 500 && code != 408 && code != 429:
		// the receiver refused it: trying again won't help
		d.State, d.Error = "failed", fmt.Sprintf("the receiver answered HTTP %d", code)
	case retry && d.Tries <= len(hookDelays):
		d.State, d.Next = "pending", time.Now().Add(hookDelays[d.Tries-1]).UnixMilli()
		if code != 0 {
			d.Error = fmt.Sprintf("the receiver answered HTTP %d", code)
		}
	default:
		d.State = "failed"
		if code != 0 {
			d.Error = fmt.Sprintf("the receiver answered HTTP %d", code)
		}
	}
	saveHooks()
}

// ---------------------------------------------------------------- final answers

// modelLabel is the model's top option as a label: "true" or "false", the option, or the level.
func modelLabel(a map[string]any) string {
	switch a["type"] {
	case "noul":
		p, _ := a["noul"].(float64)
		if p >= 0.5 {
			return "true"
		}
		return "false"
	case "choice":
		return fmt.Sprint(a["choice"])
	case "score":
		probs, _ := a["probabilities"].(map[string]any)
		keys := make([]string, 0, len(probs))
		for k := range probs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		best := ""
		for _, k := range keys {
			p, _ := probs[k].(float64)
			b, _ := probs[best].(float64)
			if best == "" || p > b {
				best = k
			}
		}
		return best
	}
	return ""
}

// typed turns a label into its value: true or false for noul, the level number for score.
func typed(kind, label string) any {
	switch kind {
	case "noul":
		return strings.EqualFold(label, "true")
	case "score":
		if n, err := strconv.Atoi(label); err == nil {
			return n
		}
	}
	return label
}

func iso(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339) }

// publicStatus is what the API calls an item's state: auto, review, reviewed or error.
func publicStatus(it *logItem) string {
	switch it.status() {
	case "waiting":
		return "review"
	case "spot":
		return "auto"
	}
	return it.status()
}

// publicItem is a logged request as the API shows it, with its final answers once it has them.
func publicItem(it *logItem) map[string]any {
	st := publicStatus(it)
	out := map[string]any{"id": it.ID, "set": it.Set, "status": st, "state": it.State, "model": it.Model, "created_at": iso(it.At), "updated_at": iso(it.At)}
	if it.Ref != "" {
		out["ref"] = it.Ref
	}
	if it.Error != "" {
		out["error"] = it.Error
		return out
	}
	out["decisions"] = it.Decisions
	keys := make([]string, 0, len(it.Answers))
	for k := range it.Answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	final, changed := map[string]any{}, []string{}
	for _, k := range keys {
		a, _ := it.Answers[k].(map[string]any)
		kind, _ := a["type"].(string)
		label := modelLabel(a)
		if it.Review != nil {
			if l, ok := it.Review.Labels[k]; ok {
				if !strings.EqualFold(l, label) {
					changed = append(changed, k)
				}
				label = l
			}
		}
		final[k] = typed(kind, label)
	}
	switch st {
	case "review":
		out["final"] = nil
	case "auto":
		out["final"], out["by"] = final, "model"
		if it.Audit {
			out["spot_check"] = true
		}
	case "reviewed":
		out["final"], out["by"], out["changed"] = final, "person", changed
		out["reviewed_at"], out["updated_at"] = iso(it.Review.At), iso(it.Review.At)
	}
	return out
}

// hookFor gives the set's webhook, if it has one.
func hookFor(slug string) (apiSet, bool) {
	apiMu.Lock()
	defer apiMu.Unlock()
	loadAPIConfig()
	if !apiCfg.Enabled {
		return apiSet{}, false
	}
	for _, s := range apiCfg.Sets {
		if s.Slug == slug && s.Webhook != "" && validHookURL(s.Webhook) {
			return s, true
		}
	}
	return apiSet{}, false
}

// afterReview sends what a person's review changed: the final answer for a request that was
// waiting, or a correction for one whose answer was already out.
func afterReview(it *logItem, before *logReview) {
	set, ok := hookFor(it.Set)
	if !ok {
		return
	}
	p := publicItem(it)
	changed, _ := p["changed"].([]string)
	switch {
	case before == nil && !it.AllAuto:
		queueHook(set, "answer.reviewed", it.ID, p, "pending")
	case before == nil:
		if len(changed) > 0 {
			queueHook(set, "answer.corrected", it.ID, p, "pending")
		}
	default:
		same := len(before.Labels) == len(it.Review.Labels)
		for k, v := range it.Review.Labels {
			if !strings.EqualFold(before.Labels[k], v) {
				same = false
			}
		}
		if !same {
			queueHook(set, "answer.corrected", it.ID, p, "pending")
		}
	}
}

// ---------------------------------------------------------------- the page's side

// handleHooks lists recent deliveries: GET /zex/hooks
func handleHooks(w http.ResponseWriter, r *http.Request, origins map[string]bool) {
	if !fromPage(w, r, origins) {
		return
	}
	hookMu.Lock()
	defer hookMu.Unlock()
	loadHooks()
	out := []*delivery{}
	for i := len(hookList) - 1; i >= 0 && len(out) < 50; i-- {
		out = append(out, hookList[i])
	}
	writeJSON(w, 200, map[string]any{"deliveries": out})
}

// handleHookSend sends now and reports how it went. With test, it pings a set's webhook:
// POST /zex/hooks/test {"set": "..."}. Otherwise it sends a delivery again: POST /zex/hooks/retry {"id": "..."}
func handleHookSend(w http.ResponseWriter, r *http.Request, origins map[string]bool, test bool) {
	if !fromPage(w, r, origins) {
		return
	}
	var in struct {
		Set string `json:"set"`
		ID  string `json:"id"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in)
	var d *delivery
	if test {
		set, ok := hookFor(in.Set)
		if !ok {
			writeJSON(w, 400, map[string]any{"error": "Zex: that set has no webhook URL"})
			return
		}
		d = queueHook(set, "ping", "", map[string]any{"set": set.Slug, "name": set.Name}, "sending")
	} else {
		hookMu.Lock()
		loadHooks()
		for _, x := range hookList {
			if x.ID == in.ID {
				d = x
			}
		}
		busy := d != nil && (d.busy || d.State == "sending")
		if d != nil && !busy {
			d.State = "sending"
		}
		hookMu.Unlock()
		if d == nil {
			writeJSON(w, 404, map[string]any{"error": "Zex: no delivery with that id"})
			return
		}
		if busy {
			writeJSON(w, 409, map[string]any{"error": "Zex: that delivery is being sent now"})
			return
		}
	}
	sendHook(d, false)
	hookMu.Lock()
	defer hookMu.Unlock()
	writeJSON(w, 200, map[string]any{"delivery": d})
}
