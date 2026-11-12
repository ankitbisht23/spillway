package resilience

import (
	"sync"
	"time"
)

// BudgetWindow is GW-13's fixed retry budget window, counted in 100 buckets
// of 100 ms (ADR-0030).
const BudgetWindow = 10 * time.Second

// budgetEpsilon absorbs float rounding in ratio × requests, so that an
// allowance that is a whole number in exact arithmetic, such as 0.2 × 15,
// is never rounded below it.
const budgetEpsilon = 1e-9

// Budget is one upstream's retry budget (GW-13, ADR-0030). A retry is
// granted at s when R(s) + 1 ≤ ratio × Q(s) + minPerSec × 10, where Q counts
// the first attempts that ended and R the retries granted over the last
// 10 s: the floor adds to the ratio. Retries never count in Q. It is safe
// for concurrent use.
type Budget struct {
	mu      sync.Mutex
	first   *Window // first attempts that ended (Q)
	retries *Window // retries granted (R)
}

// NewBudget returns a budget with nothing counted.
func NewBudget() *Budget {
	return &Budget{first: NewWindow(BudgetWindow, 100), retries: NewWindow(BudgetWindow, 100)}
}

// Request counts a first attempt that ended at now.
func (b *Budget) Request(now time.Time) {
	b.mu.Lock()
	b.first.Add(now, false)
	b.mu.Unlock()
}

// TryRetry grants a retry at now, and counts it, if the budget allows one.
func (b *Budget) TryRetry(now time.Time, ratio, minPerSec float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	q, _ := b.first.Counts(now)
	r, _ := b.retries.Counts(now)
	if float64(r+1) > ratio*float64(q)+minPerSec*BudgetWindow.Seconds()+budgetEpsilon {
		return false
	}
	b.retries.Add(now, false)
	return true
}

// Counts returns the first attempts and the retries counted over the 10 s
// that end at now.
func (b *Budget) Counts(now time.Time) (requests, retries int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	requests, _ = b.first.Counts(now)
	retries, _ = b.retries.Counts(now)
	return requests, retries
}
