package api

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// The preview of a publish and the diff between versions (UI-02, ADR-0037):
// a list of the config entities added, removed and changed, and a unified
// diff of the canonical YAML of the two configs.

// configLists are Config's entity lists, in the order changes list them.
var configLists = []struct {
	entity apiv1.ConfigChangeEntity
	field  protoreflect.Name
}{
	{"upstream", "upstreams"}, {"route", "routes"}, {"policy", "policies"}, {"plan", "plans"},
}

// configChanges lists the entities added, removed or changed from a to b,
// matched by ID, so a rename is a change; nil configs are empty. Within
// each entity type they are ordered by name.
func configChanges(a, b *configpb.Config) []apiv1.ConfigChange {
	out := []apiv1.ConfigChange{}
	for _, l := range configLists {
		before, after := byID(a, l.field), byID(b, l.field)
		var cs []apiv1.ConfigChange
		for id, m := range after {
			old, ok := before[id]
			switch {
			case !ok:
				cs = append(cs, apiv1.ConfigChange{Entity: l.entity, Id: id, Name: nameOf(m), Change: "added"})
			case !proto.Equal(old.Interface(), m.Interface()):
				cs = append(cs, apiv1.ConfigChange{Entity: l.entity, Id: id, Name: nameOf(m), Change: "changed"})
			}
		}
		for id, m := range before {
			if _, ok := after[id]; !ok {
				cs = append(cs, apiv1.ConfigChange{Entity: l.entity, Id: id, Name: nameOf(m), Change: "removed"})
			}
		}
		slices.SortFunc(cs, func(x, y apiv1.ConfigChange) int { return cmp.Or(cmp.Compare(x.Name, y.Name), cmp.Compare(x.Id, y.Id)) })
		out = append(out, cs...)
	}
	return out
}

func byID(c *configpb.Config, field protoreflect.Name) map[int64]protoreflect.Message {
	out := map[int64]protoreflect.Message{}
	if c == nil {
		return out
	}
	r := c.ProtoReflect()
	l := r.Get(r.Descriptor().Fields().ByName(field)).List()
	for i := range l.Len() {
		m := l.Get(i).Message()
		out[m.Get(m.Descriptor().Fields().ByName("id")).Int()] = m
	}
	return out
}

func nameOf(m protoreflect.Message) string {
	return m.Get(m.Descriptor().Fields().ByName("name")).String()
}

// canonicalYAML is c in export's form (ADR-0037): entities sorted by name,
// fields in proto order, only set fields, 64-bit integers as YAML integers;
// without c's version, which the diff's header names. An empty or nil
// config is no text.
func canonicalYAML(c *configpb.Config) ([]byte, error) {
	if c == nil {
		return nil, nil
	}
	c = proto.Clone(c).(*configpb.Config)
	c.Version = 0
	if proto.Size(c) == 0 {
		return nil, nil
	}
	byName := func(a, b protoreflect.Message) int { return cmp.Compare(nameOf(a), nameOf(b)) }
	r := c.ProtoReflect()
	for _, l := range configLists {
		list := r.Get(r.Descriptor().Fields().ByName(l.field)).List()
		items := make([]protoreflect.Message, list.Len())
		for i := range items {
			items[i] = list.Get(i).Message()
		}
		slices.SortStableFunc(items, byName)
		for i, m := range items {
			list.Set(i, protoreflect.ValueOfMessage(m))
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(yamlNode(r)); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// yamlNode is m as a YAML mapping of its set fields in proto order.
func yamlNode(m protoreflect.Message) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode}
	fields := m.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if !m.Has(fd) {
			continue
		}
		var v *yaml.Node
		if fd.IsList() {
			l := m.Get(fd).List()
			v = &yaml.Node{Kind: yaml.SequenceNode}
			for j := range l.Len() {
				v.Content = append(v.Content, yamlValue(fd, l.Get(j)))
			}
		} else {
			v = yamlValue(fd, m.Get(fd))
		}
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: string(fd.Name())}, v)
	}
	return n
}

func yamlValue(fd protoreflect.FieldDescriptor, v protoreflect.Value) *yaml.Node {
	if fd.Kind() == protoreflect.MessageKind {
		return yamlNode(v.Message())
	}
	var n yaml.Node
	_ = n.Encode(v.Interface()) // string, bool, int32, int64, float64: config.proto has no others
	return &n
}

// configDiff is the unified diff from a, labelled from, to b, labelled to,
// of their canonical YAML; empty when they are the same.
func configDiff(a, b *configpb.Config, from, to string) (string, error) {
	x, err := canonicalYAML(a)
	if err != nil {
		return "", err
	}
	y, err := canonicalYAML(b)
	if err != nil {
		return "", err
	}
	return unifiedDiff(from, to, lines(x), lines(y)), nil
}

func lines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// diffContext is the number of unchanged lines around each change.
const diffContext = 3

// edit is one line of an edit script: kept (' '), deleted ('-') or
// inserted ('+'); a and b are its positions in the two texts, before the
// line for the text it is not in.
type edit struct {
	op   byte
	a, b int
}

// unifiedDiff is the unified diff of a and b, lines without their
// newlines; empty when they are equal.
func unifiedDiff(from, to string, a, b []string) string {
	script := editScript(a, b)
	if !slices.ContainsFunc(script, func(e edit) bool { return e.op != ' ' }) {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", from, to)
	for i := 0; i < len(script); {
		for i < len(script) && script[i].op == ' ' {
			i++
		}
		if i == len(script) {
			break
		}
		// A hunk runs from diffContext lines before a change to
		// diffContext lines after the last change that is at most
		// 2·diffContext unchanged lines from the one before it.
		start, end := max(0, i-diffContext), i+1
		for j := i; j < len(script) && j-end < 2*diffContext; j++ {
			if script[j].op != ' ' {
				end = j + 1
			}
		}
		end = min(len(script), end+diffContext)
		hunk := script[start:end]
		na, nb := 0, 0
		for _, e := range hunk {
			if e.op != '+' {
				na++
			}
			if e.op != '-' {
				nb++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(hunk[0].a, na), hunkRange(hunk[0].b, nb))
		for _, e := range hunk {
			line := ""
			switch e.op {
			case '+':
				line = b[e.b]
			default:
				line = a[e.a]
			}
			out.WriteByte(e.op)
			out.WriteString(line)
			out.WriteByte('\n')
		}
		i = end
	}
	return out.String()
}

// hunkRange is a hunk's start line (1-based; the line before it for an
// empty range) and length.
func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

// maxEdits bounds the diff's work: past this many inserted and deleted
// lines, the middle of the texts is shown as removed and added whole.
const maxEdits = 1000

// editScript is a shortest edit script from a to b (Myers' algorithm),
// after their common prefix and suffix.
func editScript(a, b []string) []edit {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	var out []edit
	for i := range p {
		out = append(out, edit{' ', i, i})
	}
	out = append(out, myers(a[p:len(a)-s], b[p:len(b)-s], p, p)...)
	for i := range s {
		out = append(out, edit{' ', len(a) - s + i, len(b) - s + i})
	}
	return out
}

// myers returns the edits from a to b, whose lines start at offA and offB
// in the whole texts. Its memory grows with the square of the number of
// edits, which maxEdits bounds.
func myers(a, b []string, offA, offB int) []edit {
	n, m := len(a), len(b)
	if n+m == 0 {
		return nil
	}
	// v[k+off] is the furthest x on diagonal k; trace[d] is v's window
	// [-d-1, d+1] before round d.
	off := n + m + 1
	v := make([]int, 2*off+1)
	var trace [][]int
	d, found := 0, false
	for ; d <= min(n+m, maxEdits) && !found; d++ {
		trace = append(trace, slices.Clone(v[off-d-1:off+d+2]))
		for k := -d; k <= d; k += 2 {
			x := v[off+k-1] + 1
			if k == -d || k != d && v[off+k-1] < v[off+k+1] {
				x = v[off+k+1]
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[off+k] = x
			if x >= n && y >= m {
				found = true
				break
			}
		}
	}
	if !found {
		var out []edit
		for i := range n {
			out = append(out, edit{'-', offA + i, offB})
		}
		for j := range m {
			out = append(out, edit{'+', offA + n, offB + j})
		}
		return out
	}
	// Walk back from (n, m) through the rounds.
	var rev []edit
	x, y := n, m
	for d--; d > 0; d-- {
		w := trace[d] // w[k+d+1] is v[k] before round d
		k := x - y
		prevK := k - 1
		if k == -d || k != d && w[k-1+d+1] < w[k+1+d+1] {
			prevK = k + 1
		}
		prevX := w[prevK+d+1]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x, y = x-1, y-1
			rev = append(rev, edit{' ', offA + x, offB + y})
		}
		if prevK == k+1 {
			rev = append(rev, edit{'+', offA + x, offB + y - 1})
		} else {
			rev = append(rev, edit{'-', offA + x - 1, offB + y})
		}
		x, y = prevX, prevY
	}
	for x > 0 && y > 0 {
		x, y = x-1, y-1
		rev = append(rev, edit{' ', offA + x, offB + y})
	}
	slices.Reverse(rev)
	return rev
}
