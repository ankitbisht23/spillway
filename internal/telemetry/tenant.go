package telemetry

import (
	"cmp"
	"maps"
	"slices"
	"strings"
)

// otherTenant is the label of every tenant outside the busiest (OB-01).
const otherTenant = "other"

// tenantCapper keeps the tenant label to the busiest tenants (OB-01): the
// limit tenants with the most requests in the last window keep their own label, and
// the rest are "other". Until a window closes, free places go to tenants as
// they arrive. It is not safe for concurrent use.
type tenantCapper struct {
	limit  int
	counts map[string]int  // requests per tenant in this window
	top    map[string]bool // tenants with their own label
}

func newTenantCapper(limit int) *tenantCapper {
	return &tenantCapper{limit: limit, counts: map[string]int{}, top: map[string]bool{}}
}

// label counts a request from tenant and returns its label.
func (c *tenantCapper) label(tenant string) string {
	c.counts[tenant]++
	if !c.top[tenant] {
		if len(c.top) >= c.limit {
			return otherTenant
		}
		c.top[tenant] = true
	}
	return tenant
}

// rotate closes the window: the limit tenants with the most requests in it,
// ties broken by name, keep their own label in the next one. It returns the
// tenants that lose theirs.
func (c *tenantCapper) rotate() (dropped []string) {
	busiest := slices.SortedFunc(maps.Keys(c.counts), func(a, b string) int {
		return cmp.Or(cmp.Compare(c.counts[b], c.counts[a]), strings.Compare(a, b))
	})
	top := make(map[string]bool, c.limit)
	for _, t := range busiest[:min(len(busiest), c.limit)] {
		top[t] = true
	}
	for t := range c.top {
		if !top[t] {
			dropped = append(dropped, t)
		}
	}
	c.counts, c.top = map[string]int{}, top
	return dropped
}
