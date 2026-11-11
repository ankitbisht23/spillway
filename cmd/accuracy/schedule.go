package main

import (
	"cmp"
	"slices"
	"time"
)

// arrival is one scheduled request: when it is due, from the start of its
// pattern, and for which key.
type arrival struct {
	at  time.Duration
	key int
}

// patterns are the arrival patterns, in the order they run (ADR-0021). Each
// drives every key at twice the harness policy's limit: 200 requests per
// second per key.
var patterns = []string{"uniform", "bursty", "boundary"}

// schedule returns pattern's arrivals for keys keys over d, sorted by due
// time (ADR-0021):
//   - uniform: key k sends every 5 ms, offset by k·0.5 ms;
//   - bursty: key k sends 100 requests at once every 500 ms, offset by k·50 ms;
//   - boundary: around every whole second s, every key sends 100 requests
//     evenly spread over the 10 ms before s and 100 over the 10 ms after it.
//
// Arrivals outside [0, d) are left out, so for whole seconds d every key gets
// 200·d requests.
func schedule(pattern string, keys int, d time.Duration) []arrival {
	var a []arrival
	add := func(at time.Duration, key int) {
		if at >= 0 && at < d {
			a = append(a, arrival{at, key})
		}
	}
	for k := range keys {
		switch pattern {
		case "uniform":
			for t := time.Duration(k) * 500 * time.Microsecond; t < d; t += 5 * time.Millisecond {
				add(t, k)
			}
		case "bursty":
			for t := time.Duration(k) * 50 * time.Millisecond; t < d; t += 500 * time.Millisecond {
				for range 100 {
					add(t, k)
				}
			}
		case "boundary":
			for s := time.Duration(0); s <= d; s += time.Second {
				for i := range time.Duration(100) {
					add(s-10*time.Millisecond+i*100*time.Microsecond, k)
					add(s+i*100*time.Microsecond, k)
				}
			}
		}
	}
	slices.SortStableFunc(a, func(x, y arrival) int { return cmp.Compare(x.at, y.at) })
	return a
}
