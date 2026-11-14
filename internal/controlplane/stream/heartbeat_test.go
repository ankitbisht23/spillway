package stream

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// fakeRecorder records heartbeats in memory.
type fakeRecorder struct {
	mu     sync.Mutex
	got    []*configpb.HeartbeatRequest
	names  []string
	live   int32
	fail   error
	prunes int
}

func (f *fakeRecorder) RecordHeartbeat(_ context.Context, replica string, hb *configpb.HeartbeatRequest) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return 0, f.fail
	}
	f.names = append(f.names, replica)
	f.got = append(f.got, hb)
	return f.live, nil
}

func (f *fakeRecorder) PruneReplicas(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prunes++
	return 0, f.fail
}

func (f *fakeRecorder) pruned() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prunes
}

func (f *fakeRecorder) set(live int32, fail error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live, f.fail = live, fail
}

// TestHeartbeat checks Heartbeat (CP-08, RL-24, ADR-0038): the request is
// recorded under its replica name as sent, the answer is the recorder's
// live count, at least 1; a name that is not a DNS label is
// INVALID_ARGUMENT; a failed recording is UNAVAILABLE.
func TestHeartbeat(t *testing.T) {
	rec := &fakeRecorder{live: 3}
	client, _ := configServer(t, newFakeSource(1, 1), rec)
	ctx := t.Context()
	hb := &configpb.HeartbeatRequest{
		ReplicaId: "gateway-2", ConfigVersion: 7, IdentityVersion: 9, SchemaVersion: currentSchema(),
		WatchConnected: true, RejectedConfigVersion: 8, RejectedReason: "route \"r\": upstream \"u\" is not defined",
	}
	resp, err := client.Heartbeat(ctx, hb)
	if err != nil || resp.GetLiveReplicas() != 3 {
		t.Fatalf("Heartbeat: %v, %v", resp, err)
	}
	if len(rec.got) != 1 || rec.names[0] != "gateway-2" || !proto.Equal(rec.got[0], hb) {
		t.Fatalf("recorded %v %v", rec.names, rec.got)
	}

	rec.set(0, nil) // never below 1: the caller is live
	if resp, err := client.Heartbeat(ctx, hb); err != nil || resp.GetLiveReplicas() != 1 {
		t.Errorf("with 0 live: %v, %v", resp, err)
	}
	for _, name := range []string{"", "Gateway-1", "gateway_1", strings.Repeat("a", 64)} {
		_, err := client.Heartbeat(ctx, &configpb.HeartbeatRequest{ReplicaId: name})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("replica_id %q: %v, want INVALID_ARGUMENT", name, err)
		}
	}
	rec.set(0, errors.New("connection refused"))
	if _, err := client.Heartbeat(ctx, hb); status.Code(err) != codes.Unavailable {
		t.Errorf("recording failed: %v, want UNAVAILABLE", err)
	}
}

// TestHeartbeatErrorsLoggedOncePerMinute checks that failing heartbeats
// log at most once a minute.
func TestHeartbeatErrorsLoggedOncePerMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		s := NewService(nil, &fakeRecorder{fail: errors.New("connection refused")}, slog.New(slog.NewTextHandler(&buf, nil)))
		hb := &configpb.HeartbeatRequest{ReplicaId: "gateway-1"}
		for range 13 { // 60 s of heartbeats every 5 s
			if _, err := s.Heartbeat(t.Context(), hb); status.Code(err) != codes.Unavailable {
				t.Fatalf("%v, want UNAVAILABLE", err)
			}
			time.Sleep(5 * time.Second)
		}
		if n := strings.Count(buf.String(), "recording a heartbeat failed"); n != 2 {
			t.Errorf("logged %d times over 65 s, want 2:\n%s", n, buf.String())
		}
	})
}

// TestPrune checks the prune's schedule (CP-22, ADR-0038): once on
// becoming active, then every hour, until ctx ends; a failure waits for
// the next hour.
func TestPrune(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &fakeRecorder{fail: errors.New("connection refused")}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			prune(ctx, rec, discard())
		}()
		synctest.Wait()
		if rec.pruned() != 1 {
			t.Fatalf("%d prunes on becoming active", rec.pruned())
		}
		time.Sleep(time.Hour - time.Second)
		synctest.Wait()
		if rec.pruned() != 1 {
			t.Fatalf("%d prunes before the hour", rec.pruned())
		}
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		if rec.pruned() != 3 {
			t.Errorf("%d prunes after 2 h 59 min 59 s, want 3", rec.pruned())
		}
		cancel()
		<-done
	})
}
