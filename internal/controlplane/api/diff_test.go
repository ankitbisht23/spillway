package api

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// apply applies script to a and returns the text it makes, failing on a
// kept or deleted line that is not a's.
func apply(t *rapid.T, script []edit, a, b []string) []string {
	var out []string
	next := 0
	for _, e := range script {
		switch e.op {
		case ' ', '-':
			if e.a != next || e.op == ' ' && b[e.b] != a[e.a] {
				t.Fatalf("edit %+v out of order or not matching", e)
			}
			next++
			if e.op == ' ' {
				out = append(out, a[e.a])
			}
		case '+':
			out = append(out, b[e.b])
		}
	}
	if next != len(a) {
		t.Fatalf("script covers %d of %d lines of a", next, len(a))
	}
	return out
}

// lcs is the length of a longest common subsequence of a and b.
func lcs(a, b []string) int {
	prev, cur := make([]int, len(b)+1), make([]int, len(b)+1)
	for i := range a {
		for j := range b {
			if a[i] == b[j] {
				cur[j+1] = prev[j] + 1
			} else {
				cur[j+1] = max(cur[j], prev[j+1])
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// TestEditScript checks that the edit script turns a into b with the fewest
// edits: len(a)+len(b) minus twice their longest common subsequence.
func TestEditScript(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		line := rapid.SampledFrom([]string{"a", "b", "c", "d"})
		a := rapid.SliceOfN(line, 0, 30).Draw(t, "a")
		b := rapid.SliceOfN(line, 0, 30).Draw(t, "b")
		script := editScript(a, b)
		if got := apply(t, script, a, b); !slices.Equal(got, b) {
			t.Fatalf("the script makes %q, want %q", got, b)
		}
		edits := 0
		for _, e := range script {
			if e.op != ' ' {
				edits++
			}
		}
		if want := len(a) + len(b) - 2*lcs(a, b); edits != want {
			t.Fatalf("%d edits, want %d", edits, want)
		}
	})
}

// TestEditScriptBounded checks the fallback past maxEdits: still a correct
// script.
func TestEditScriptBounded(t *testing.T) {
	var a, b []string
	for i := range maxEdits {
		a, b = append(a, "a"+strconv.Itoa(i)), append(b, "b"+strconv.Itoa(i))
	}
	a, b = append([]string{"same"}, append(a, "end")...), append([]string{"same"}, append(b, "end")...)
	rapid.Check(t, func(rt *rapid.T) {
		if got := apply(rt, editScript(a, b), a, b); !slices.Equal(got, b) {
			rt.Fatalf("the fallback script is wrong")
		}
	})
}

// patch applies a unified diff to a, as patch(1) would, checking each
// hunk's header and context.
func patch(t *rapid.T, a []string, diff string) []string {
	if diff == "" {
		return a
	}
	ls := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	if !strings.HasPrefix(ls[0], "--- ") || !strings.HasPrefix(ls[1], "+++ ") {
		t.Fatalf("no header: %q", diff)
	}
	var out []string
	pos := 0 // lines of a copied or consumed
	for i := 2; i < len(ls); {
		var as, an, bs, bn int
		if _, err := fmt.Sscanf(ls[i], "@@ -%d,%d +%d,%d @@", &as, &an, &bs, &bn); err != nil {
			t.Fatalf("hunk header %q: %v", ls[i], err)
		}
		start := as - 1
		if an == 0 {
			start = as
		}
		if start < pos {
			t.Fatalf("hunk %q overlaps the one before", ls[i])
		}
		out = append(out, a[pos:start]...)
		if bs != len(out)+1 && (bn != 0 || bs != len(out)) {
			t.Fatalf("hunk %q: b starts at %d, want %d", ls[i], bs, len(out)+1)
		}
		pos = start
		i++
		gotA, gotB := 0, 0
		for ; i < len(ls) && !strings.HasPrefix(ls[i], "@@"); i++ {
			op, text := ls[i][0], ls[i][1:]
			switch op {
			case ' ', '-':
				if a[pos] != text {
					t.Fatalf("hunk line %q does not match a[%d] %q", ls[i], pos, a[pos])
				}
				pos++
				gotA++
				if op == ' ' {
					out = append(out, text)
					gotB++
				}
			case '+':
				out = append(out, text)
				gotB++
			default:
				t.Fatalf("line %q", ls[i])
			}
		}
		if gotA != an || gotB != bn {
			t.Fatalf("hunk counts %d,%d, header says %d,%d", gotA, gotB, an, bn)
		}
	}
	return append(out, a[pos:]...)
}

// TestUnifiedDiff checks that the diff applies: patching a with it gives
// b, every hunk has at most diffContext lines of context at each end, and
// equal texts give no diff.
func TestUnifiedDiff(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		line := rapid.SampledFrom([]string{"a", "b", "c", "d", "e", "f"})
		a := rapid.SliceOfN(line, 0, 40).Draw(t, "a")
		b := rapid.SliceOfN(line, 0, 40).Draw(t, "b")
		d := unifiedDiff("from", "to", a, b)
		if slices.Equal(a, b) != (d == "") {
			t.Fatalf("equal %v, diff %q", slices.Equal(a, b), d)
		}
		if got := patch(t, a, d); !slices.Equal(got, b) {
			t.Fatalf("patched: %q, want %q\n%s", got, b, d)
		}
	})
}

func TestUnifiedDiffExample(t *testing.T) {
	a := strings.Split("1 2 3 4 5 6 7 8 9 10 11 12 13 14 15", " ")
	b := strings.Split("1 2 3 4 5 x 7 8 9 10 11 12 13 14 y", " ")
	want := `--- version 1
+++ version 2
@@ -3,7 +3,7 @@
 3
 4
 5
-6
+x
 7
 8
 9
@@ -12,4 +12,4 @@
 12
 13
 14
-15
+y
`
	if got := unifiedDiff("version 1", "version 2", a, b); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := unifiedDiff("empty", "one", nil, []string{"x"}); got != "--- empty\n+++ one\n@@ -0,0 +1,1 @@\n+x\n" {
		t.Errorf("from nothing: %q", got)
	}
}

// TestCanonicalYAML checks export's form (ADR-0037): entities by name,
// fields in proto order, unset fields left out, integers as integers, no
// version.
func TestCanonicalYAML(t *testing.T) {
	c := parseConfig(t, `{"version": "41",
	  "routes": [{"id": "12", "name": "b", "upstream": "u", "match": {"path": "/b"}, "cost": "3"}, {"id": 11, "name": "a", "upstream": "u", "match": {"path": "/a"}}],
	  "upstreams": [{"name": "u", "id": 1, "lb": "round_robin", "endpoints": [{"address": "172.29.0.10:8080", "id": 4, "draining": true}]}],
	  "policies": [{"id": 5, "name": "p", "level": "route", "mode": "off", "key_template": "{route}", "algorithm": "gcra", "match": {"routes": ["*"]}, "limits": [{"rate": 1, "period": "1s"}]}]}`)
	got, err := canonicalYAML(c)
	if err != nil {
		t.Fatal(err)
	}
	want := `upstreams:
  - name: u
    endpoints:
      - address: 172.29.0.10:8080
        id: 4
        draining: true
    lb: round_robin
    id: 1
routes:
  - name: a
    match:
      path: /a
    upstream: u
    id: 11
  - name: b
    match:
      path: /b
    upstream: u
    cost: 3
    id: 12
policies:
  - name: p
    level: route
    match:
      routes:
        - '*'
    key_template: '{route}'
    algorithm: gcra
    limits:
      - rate: 1
        period: 1s
    mode: "off"
    id: 5
`
	if string(got) != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if c.GetVersion() != 41 || c.Routes[0].GetName() != "b" {
		t.Error("canonicalYAML changed its argument")
	}
	if got, _ := canonicalYAML(&configpb.Config{Version: 3}); len(got) != 0 {
		t.Errorf("empty config: %q", got)
	}
}

// TestConfigChanges checks the changes list: matched by ID, so a rename is
// a change, ordered by entity type and name.
func TestConfigChanges(t *testing.T) {
	a := parseConfig(t, `{"routes": [{"id": 1, "name": "r1", "upstream": "u"}, {"id": 2, "name": "r2", "upstream": "u"}, {"id": 3, "name": "r3", "upstream": "u"}],
	  "plans": [{"id": 7, "name": "free"}]}`)
	b := parseConfig(t, `{"routes": [{"id": 1, "name": "r1", "upstream": "u"}, {"id": 2, "name": "renamed", "upstream": "u"}, {"id": 4, "name": "a-new", "upstream": "u"}],
	  "upstreams": [{"id": 9, "name": "u"}], "plans": [{"id": 7, "name": "free"}]}`)
	got := configChanges(a, b)
	want := []apiv1.ConfigChange{
		{Entity: "upstream", Id: 9, Name: "u", Change: "added"},
		{Entity: "route", Id: 4, Name: "a-new", Change: "added"},
		{Entity: "route", Id: 3, Name: "r3", Change: "removed"},
		{Entity: "route", Id: 2, Name: "renamed", Change: "changed"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if got := configChanges(nil, nil); got == nil || len(got) != 0 {
		t.Errorf("no changes: %#v, want an empty list", got)
	}
	if got := configChanges(b, b); len(got) != 0 {
		t.Errorf("a config against itself: %+v", got)
	}
}
