package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRevocation is a control plane and gateways for "chaos revocation":
// a key works on a gateway until its revocation plus that gateway's delay.
type fakeRevocation struct {
	cp       *httptest.Server
	gateways []*httptest.Server
	mu       sync.Mutex
	revoked  map[string]time.Time // key -> when
	keys     map[int64]string
	next     int64
	back     bool // a gateway answers 200 again after its first 401
}

func newFakeRevocation(t *testing.T, delays ...time.Duration) *fakeRevocation {
	t.Helper()
	f := &fakeRevocation{revoked: map[string]time.Time{}, keys: map[int64]string{}}
	f.cp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		var id int64
		_, _ = fmt.Sscanf(r.URL.Path, "/api/v1/keys/%d/revoke", &id)
		switch {
		case r.URL.Path == "/api/v1/consumers/7/keys":
			f.next++
			key := fmt.Sprintf("test-key-%d", f.next)
			f.keys[f.next] = key
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": f.next, "key": key})
		case id > 0:
			f.revoked[f.keys[id]] = time.Now()
			_, _ = io.WriteString(w, "{}")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.cp.Close)
	for _, d := range delays {
		var once sync.Map
		g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-Api-Key")
			f.mu.Lock()
			at, ok := f.revoked[key]
			back := f.back
			f.mu.Unlock()
			if ok && time.Since(at) >= d {
				if _, seen := once.LoadOrStore(key, true); seen && back {
					return // 200
				}
				w.WriteHeader(http.StatusUnauthorized)
			}
		}))
		t.Cleanup(g.Close)
		f.gateways = append(f.gateways, g)
	}
	return f
}

func (f *fakeRevocation) args(rounds int) []string {
	var gs []string
	for _, g := range f.gateways {
		gs = append(gs, strings.TrimPrefix(g.URL, "http://"))
	}
	return []string{"-controlplane", f.cp.URL + "/api/v1", "-consumer", "7", "-gateways", strings.Join(gs, ","),
		"-rounds", fmt.Sprint(rounds), "-limit", "1s", "-every", "10ms", "-after", "3"}
}

func TestRevocation(t *testing.T) {
	f := newFakeRevocation(t, 0, 100*time.Millisecond, 200*time.Millisecond)
	var out, errOut strings.Builder
	if code := runRevocation(t.Context(), f.args(3), strings.NewReader("tok\n"), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s %s", code, errOut.String(), out.String())
	}
	var rep revocationReport
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Pass || len(rep.Rounds) != 3 || rep.MaxMS < 200 || rep.MaxMS >= 1000 || len(rep.Rounds[0].RejectedMS) != 3 {
		t.Errorf("report %+v", rep)
	}
	if !strings.HasPrefix(errOut.String(), "PASS revocation rejected on all 3 replicas: 3 of 3 rounds") {
		t.Errorf("stderr %q", errOut.String())
	}
	if strings.Contains(out.String()+errOut.String(), "test-key-") || strings.Contains(out.String()+errOut.String(), "tok") {
		t.Error("a key or the token was printed")
	}
}

func TestRevocationFails(t *testing.T) {
	slow := newFakeRevocation(t, 0, 1200*time.Millisecond)
	var out, errOut strings.Builder
	if code := runRevocation(t.Context(), slow.args(1), strings.NewReader("tok\n"), &out, &errOut); code != 1 || !strings.HasPrefix(errOut.String(), "FAIL") {
		t.Errorf("a replica over the limit: exit %d, %s", code, errOut.String())
	}
	back := newFakeRevocation(t, 0)
	back.back = true
	out.Reset()
	errOut.Reset()
	if code := runRevocation(t.Context(), back.args(1), strings.NewReader("tok\n"), &out, &errOut); code != 1 || !strings.Contains(out.String(), "the key came back") {
		t.Errorf("a key that came back: exit %d, %s", code, out.String())
	}
	if code := runRevocation(t.Context(), back.args(1), strings.NewReader(""), io.Discard, io.Discard); code != 2 {
		t.Errorf("no token: exit %d, want 2", code)
	}
	if code := runRevocation(t.Context(), []string{"-rounds", "1"}, strings.NewReader("tok\n"), io.Discard, io.Discard); code != 2 {
		t.Errorf("no -consumer: exit %d, want 2", code)
	}
}
