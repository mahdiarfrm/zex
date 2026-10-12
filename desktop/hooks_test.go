package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type hookCall struct {
	Event, Delivery, Signature string
	Body                       []byte
}

// receiver records webhook deliveries; fail makes it answer with that status instead of 200.
func receiver(t *testing.T) (*httptest.Server, chan hookCall, *atomic.Int32) {
	t.Helper()
	calls, fail := make(chan hookCall, 20), &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls <- hookCall{r.Header.Get("X-Zex-Event"), r.Header.Get("X-Zex-Delivery"), r.Header.Get("X-Zex-Signature"), body}
		if code := fail.Load(); code != 0 {
			w.WriteHeader(int(code))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, calls, fail
}

func next(t *testing.T, calls chan hookCall) hookCall {
	t.Helper()
	select {
	case c := <-calls:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook delivery arrived")
	}
	return hookCall{}
}

func verify(t *testing.T, secret string, c hookCall) map[string]any {
	t.Helper()
	parts := map[string]string{}
	for _, p := range strings.Split(c.Signature, ",") {
		k, v, _ := strings.Cut(p, "=")
		parts[k] = v
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(parts["t"] + "."))
	m.Write(c.Body)
	if parts["v1"] != hex.EncodeToString(m.Sum(nil)) {
		t.Fatalf("bad signature %q for %s", c.Signature, c.Body)
	}
	var out map[string]any
	json.Unmarshal(c.Body, &out)
	return out
}

func TestWebhooksAndResults(t *testing.T) {
	var path string
	up := fakeModel(t, &path)
	defer up.Close()
	rx, calls, _ := receiver(t)
	h := setup(t)
	cfg := configure(t, h, `{"enabled":true,"model":"laya:latest","thr":0.8,
		"connection":{"kind":"ollaya","name":"Ollaya","url":"`+up.URL+`"},
		"sets":[{"slug":"sure","name":"Sure","questions":{"team":{"type":"choice"},"refund":{"type":"noul"}},"webhook":"`+rx.URL+`/hook","events":"all"},
		        {"slug":"unsure","name":"Unsure","questions":{"team":{"type":"choice"},"refund":{"type":"noul"},"mood":{"type":"score","criteria":["calm","annoyed","angry"]}},"webhook":"`+rx.URL+`/hook"}]}`)
	secret, _ := cfg["hook_secret"].(string)
	if !strings.HasPrefix(secret, "zexwh_") {
		t.Fatalf("no webhook secret: %v", cfg)
	}
	auth := map[string]string{"Authorization": "Bearer " + cfg["token"].(string), "Content-Type": "application/json"}
	page := map[string]string{"X-Zex": "1", "Origin": "http://127.0.0.1:18765", "Content-Type": "application/json"}

	// a sure answer comes back with its final values and goes to the webhook at once
	r := get(t, h, "POST", "/api/sets/sure", auth, `{"text":"I was charged twice","ref":"ticket-7"}`)
	var res map[string]any
	json.Unmarshal(r.Body.Bytes(), &res)
	if res["status"] != "auto" || res["ref"] != "ticket-7" || res["webhook"] != true {
		t.Fatalf("sure run: %s", r.Body)
	}
	if f, _ := res["final"].(map[string]any); f["team"] != "billing" || f["refund"] != true {
		t.Fatalf("final: %v", res["final"])
	}
	c := next(t, calls)
	ev := verify(t, secret, c)
	if c.Event != "answer.auto" || ev["event"] != "answer.auto" || ev["id"] != res["id"] || ev["ref"] != "ticket-7" || ev["by"] != "model" || ev["delivery"] != c.Delivery {
		t.Fatalf("auto event: %s %s", c.Event, c.Body)
	}

	// an unsure one waits for a person; nothing is sent until the review
	r = get(t, h, "POST", "/api/sets/unsure", auth, `{"text":"Fine, I guess","ref":"ticket-8"}`)
	json.Unmarshal(r.Body.Bytes(), &res)
	id, _ := res["id"].(string)
	if res["status"] != "review" || res["final"] != nil {
		t.Fatalf("unsure run: %s", r.Body)
	}
	r = get(t, h, "GET", "/api/requests/"+id, auth, "")
	var item map[string]any
	json.Unmarshal(r.Body.Bytes(), &item)
	if r.Code != 200 || item["status"] != "review" || item["final"] != nil || item["ref"] != "ticket-8" {
		t.Fatalf("waiting item: %d %s", r.Code, r.Body)
	}
	select {
	case c := <-calls:
		t.Fatalf("nothing should be sent before the review, got %s", c.Body)
	case <-time.After(150 * time.Millisecond):
	}

	if r := get(t, h, "POST", "/zex/review", page, `{"id":"`+id+`","labels":{"team":"billing","refund":"false","mood":"2"},"checked":3,"agreed":1}`); r.Code != 200 {
		t.Fatalf("review: %d %s", r.Code, r.Body)
	}
	c = next(t, calls)
	ev = verify(t, secret, c)
	f, _ := ev["final"].(map[string]any)
	if c.Event != "answer.reviewed" || ev["by"] != "person" || f["team"] != "billing" || f["refund"] != false || f["mood"] != 2.0 {
		t.Fatalf("reviewed event: %s %s", c.Event, c.Body)
	}
	if ch, _ := ev["changed"].([]any); len(ch) != 2 || ch[0] != "mood" || ch[1] != "refund" {
		t.Fatalf("changed: %v", ev["changed"])
	}
	// the same answers again change nothing; a different one is a correction
	get(t, h, "POST", "/zex/review", page, `{"id":"`+id+`","labels":{"team":"billing","refund":"false","mood":"2"},"checked":3,"agreed":1}`)
	get(t, h, "POST", "/zex/review", page, `{"id":"`+id+`","labels":{"team":"technical","refund":"false","mood":"2"},"checked":3,"agreed":0}`)
	if c = next(t, calls); c.Event != "answer.corrected" {
		t.Fatalf("want a correction, got %s", c.Event)
	}

	r = get(t, h, "GET", "/api/requests/"+id, auth, "")
	json.Unmarshal(r.Body.Bytes(), &item)
	if f, _ := item["final"].(map[string]any); item["status"] != "reviewed" || f["team"] != "technical" || item["reviewed_at"] == nil {
		t.Fatalf("reviewed item: %s", r.Body)
	}
	r = get(t, h, "GET", "/api/requests?status=reviewed", auth, "")
	var list struct{ Requests []map[string]any }
	json.Unmarshal(r.Body.Bytes(), &list)
	if len(list.Requests) != 1 || list.Requests[0]["id"] != id {
		t.Fatalf("list: %s", r.Body)
	}
	r = get(t, h, "GET", "/api/requests?since="+time.Now().Add(time.Minute).UTC().Format(time.RFC3339), auth, "")
	json.Unmarshal(r.Body.Bytes(), &list)
	if len(list.Requests) != 0 {
		t.Fatalf("nothing changed after a minute from now: %s", r.Body)
	}
	if r := get(t, h, "GET", "/api/requests/r_nope", auth, ""); r.Code != 404 {
		t.Fatalf("unknown id: %d", r.Code)
	}
	r = get(t, h, "GET", "/api/sets/unsure", auth, "")
	var set map[string]any
	json.Unmarshal(r.Body.Bytes(), &set)
	if q, _ := set["questions"].(map[string]any); len(q) != 3 || set["webhook"] != true {
		t.Fatalf("set: %s", r.Body)
	}

	// the page can ping a webhook and see each delivery
	r = get(t, h, "POST", "/zex/hooks/test", page, `{"set":"unsure"}`)
	if !strings.Contains(r.Body.String(), `"state":"sent"`) {
		t.Fatalf("test ping: %s", r.Body)
	}
	if c = next(t, calls); c.Event != "ping" {
		t.Fatalf("want a ping, got %s", c.Event)
	}
	r = get(t, h, "GET", "/zex/hooks", page, "")
	var hooks struct{ Deliveries []delivery }
	json.Unmarshal(r.Body.Bytes(), &hooks)
	if len(hooks.Deliveries) != 4 || hooks.Deliveries[0].Event != "ping" || hooks.Deliveries[3].Event != "answer.auto" {
		t.Fatalf("deliveries: %s", r.Body)
	}
}

func TestWebhookRetries(t *testing.T) {
	var path string
	up := fakeModel(t, &path)
	defer up.Close()
	rx, calls, fail := receiver(t)
	h := setup(t)
	saved := hookDelays
	hookDelays = []time.Duration{50 * time.Millisecond, 50 * time.Millisecond}
	t.Cleanup(func() { hookDelays = saved })
	cfg := configure(t, h, `{"enabled":true,"model":"laya:latest","thr":0.8,
		"connection":{"kind":"ollaya","name":"Ollaya","url":"`+up.URL+`"},
		"sets":[{"slug":"sure","name":"Sure","questions":{"team":{"type":"choice"}},"webhook":"`+rx.URL+`","events":"all"}]}`)
	auth := map[string]string{"Authorization": "Bearer " + cfg["token"].(string), "Content-Type": "application/json"}
	page := map[string]string{"X-Zex": "1", "Origin": "http://127.0.0.1:18765", "Content-Type": "application/json"}
	state := func() (d delivery) {
		hookMu.Lock()
		defer hookMu.Unlock()
		if len(hookList) > 0 {
			d = *hookList[len(hookList)-1]
		}
		return
	}
	waitFor := func(want string, tries int) delivery {
		t.Helper()
		for i := 0; i < 100; i++ {
			if d := state(); d.State == want && d.Tries == tries {
				return d
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("delivery never reached %s after %d tries: %+v", want, tries, state())
		return delivery{}
	}

	// down at first, then up: the second try gets through
	fail.Store(503)
	get(t, h, "POST", "/api/sets/sure", auth, `{"text":"one"}`)
	next(t, calls)
	fail.Store(0)
	next(t, calls)
	waitFor("sent", 2)

	// still down after every try: it fails, and sending it again by hand works
	fail.Store(500)
	get(t, h, "POST", "/api/sets/sure", auth, `{"text":"two"}`)
	d := waitFor("failed", 3)
	fail.Store(0)
	r := get(t, h, "POST", "/zex/hooks/retry", page, `{"id":"`+d.ID+`"}`)
	if !strings.Contains(r.Body.String(), `"state":"sent"`) {
		t.Fatalf("retry: %s", r.Body)
	}

	// a refusal isn't tried again
	fail.Store(404)
	get(t, h, "POST", "/api/sets/sure", auth, `{"text":"three"}`)
	d = waitFor("failed", 1)
	if !strings.Contains(d.Error, "404") {
		t.Fatalf("error: %q", d.Error)
	}
}
