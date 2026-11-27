package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/ankitbisht23/spillway/internal/controlplane/demo"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// The Playground's operations (UI-07, SC-04, ADR-0096 §3), under the demo
// guard: start a run (every role, P46), stop one's own run, and one's own
// run's results over SSE, served by its own handler as GET /stream is.

// openPlaygroundStreamID is the operation the run-stream handler serves.
const openPlaygroundStreamID = "openPlaygroundStream"

// playgroundActor is the actor of the Playground keys' audit records
// (ADR-0095 §3).
var playgroundActor = store.Actor{Sub: "system:playground", Name: "Playground"}

// WithDemoGateways sets the demo gateways the Playground sends to, round
// robin, such as http://gateway-1:8000 (ADR-0095 §1); call it after
// WithDemo. Without it the Playground answers 503.
func (s *Server) WithDemoGateways(gateways []string) *Server {
	s.playground = demo.NewPlayground(demo.PlaygroundConfig{Gateways: gateways, Keys: s.demoKeys, Store: playgroundStore{s}, Log: s.log})
	return s
}

// ShutdownPlayground ends every Playground run and stream; register it
// with http.Server.RegisterOnShutdown.
func (s *Server) ShutdownPlayground() {
	if s.playground != nil {
		s.playground.Shutdown()
	}
}

// playgroundStore issues the Playground's keys through the store as
// system:playground, each one identity change with its audit record (the
// key's ID and prefix, never the key).
type playgroundStore struct{ s *Server }

func (p playgroundStore) PlaygroundConsumer(ctx context.Context, tenant string) (int64, error) {
	tenants, err := p.s.store.ListTenants(ctx, store.Page{Limit: 1}, &tenant)
	if err != nil {
		return 0, err
	}
	if len(tenants) == 0 {
		return 0, demo.ErrNoConsumer
	}
	name := tenant + "-playground"
	consumers, err := p.s.store.ListConsumers(ctx, store.Page{Limit: 1}, &tenants[0].ID, &name)
	if err != nil {
		return 0, err
	}
	if len(consumers) == 0 {
		return 0, demo.ErrNoConsumer
	}
	return consumers[0].ID, nil
}

func (p playgroundStore) IssueKey(ctx context.Context, consumerID int64, expires *time.Time) (int64, secret.Value, error) {
	a := store.Audit{Actor: playgroundActor, RequestID: "playground-key-" + uuid.NewString(), Show: showKey}
	row, key, err := p.s.store.IssueKey(ctx, a, p.s.newKey, consumerID, expires)
	return row.ID, key.Secret, err
}

type clientIPKey struct{}

// withClientIP keeps the address the Playground's per-IP caps count a
// request under: the TCP peer's, IPv6 per /64 (ADR-0096 §3, ADR-0097 §2).
// It is never logged, stored or audited.
func withClientIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, clientIP(r.RemoteAddr))))
	})
}

// clientIP is addr's host, an IPv6 address as its /64.
func clientIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	ip = ip.Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}

func callerOf(ctx context.Context) demo.Caller {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return demo.Caller{Sub: actorOf(ctx).Sub, IP: ip}
}

// playgroundOf is the caller's Playground bounds and use for getDemo.
func (s *Server) playgroundOf(ctx context.Context) apiv1.DemoPlayground {
	out := apiv1.DemoPlayground{MaxRate: demo.MaxRate, MaxDurationS: int32(demo.MaxDuration / time.Second)}
	if s.playground != nil {
		active, left := s.playground.Usage(callerOf(ctx))
		out.ActiveRuns, out.RunsLeftThisHour = int32(active), int32(left) //nolint:gosec // G115: at most 3 and 120
	}
	return out
}

// playgroundShape checks a start's body and returns its shape: a missing
// field, a value outside its range or a field its kind does not take is
// 422 naming the field; an unknown kind 400.
func playgroundShape(b *apiv1.PlaygroundRunRequest) (demo.Shape, error) {
	if b == nil {
		return demo.Shape{}, invalid("body: required")
	}
	field := func(name, msg string) error { return validation("playground_run", name, name+": "+msg) }
	notTaken := func(kind string, fields map[string]bool) error {
		for _, name := range []string{"tenant", "route", "rate", "duration_s", "scenario"} {
			if fields[name] {
				return field(name, "not taken by a "+kind+" run")
			}
		}
		return nil
	}
	switch b.Kind {
	case demo.KindCustom:
		if err := notTaken("custom", map[string]bool{"scenario": b.Scenario != nil}); err != nil {
			return demo.Shape{}, err
		}
		switch {
		case b.Tenant == nil:
			return demo.Shape{}, field("tenant", "required for a custom run")
		case !slices.Contains(demo.PlaygroundTenants, string(*b.Tenant)):
			return demo.Shape{}, field("tenant", "want free-co, pro-co or enterprise-co")
		case b.Route == nil:
			return demo.Shape{}, field("route", "required for a custom run")
		case !slices.Contains(demo.PlaygroundRoutes, string(*b.Route)):
			return demo.Shape{}, field("route", "want orders, catalog or payments")
		case b.Rate == nil:
			return demo.Shape{}, field("rate", "required for a custom run")
		case *b.Rate < 1 || *b.Rate > demo.MaxRate:
			return demo.Shape{}, field("rate", fmt.Sprintf("want 1 to %d requests per second", demo.MaxRate))
		}
		d := demo.DefaultDuration
		if b.DurationS != nil {
			d = time.Duration(*b.DurationS) * time.Second
			if d < demo.MinDuration || d > demo.MaxDuration {
				return demo.Shape{}, field("duration_s", fmt.Sprintf("want %d to %d seconds", int(demo.MinDuration/time.Second), int(demo.MaxDuration/time.Second)))
			}
		}
		return demo.CustomShape(string(*b.Tenant), string(*b.Route), int(*b.Rate), d), nil
	case demo.KindScenario:
		if err := notTaken("scenario", map[string]bool{"tenant": b.Tenant != nil, "route": b.Route != nil, "rate": b.Rate != nil, "duration_s": b.DurationS != nil}); err != nil {
			return demo.Shape{}, err
		}
		if b.Scenario == nil {
			return demo.Shape{}, field("scenario", "required for a scenario run")
		}
		shape, ok := demo.ScenarioShape(string(*b.Scenario))
		if !ok {
			return demo.Shape{}, field("scenario", "want d1, d2 or d3")
		}
		return shape, nil
	}
	return demo.Shape{}, invalid("body: kind: want custom or scenario")
}

// runDiff is a run's shape as its audit records hold it.
func runDiff(in demo.Info) map[string]any {
	return map[string]any{"kind": in.Kind, "tenant": in.Tenant, "route": in.Route, "rate": in.Rate, "duration_s": in.DurationS, "scenario": in.Scenario}
}

func runOut(in demo.Info) apiv1.PlaygroundRun {
	return apiv1.PlaygroundRun{
		Id: in.ID, Kind: apiv1.PlaygroundRunKind(in.Kind), Tenant: in.Tenant, Route: in.Route, Rate: in.Rate, Scenario: in.Scenario,
		DurationS: in.DurationS, StartedAt: in.StartedAt, EndsAt: in.EndsAt, State: apiv1.PlaygroundRunState(in.State),
	}
}

// errNoPlayground answers when the control plane has no demo gateways.
var errNoPlayground = &apiError{http.StatusServiceUnavailable, apiv1.ErrorCodeUnavailable, "the Playground has no demo gateways", nil}

// StartPlaygroundRun starts a run for the caller and writes its audit
// record (ADR-0096 §3).
func (s *Server) StartPlaygroundRun(ctx context.Context, req apiv1.StartPlaygroundRunRequestObject) (apiv1.StartPlaygroundRunResponseObject, error) {
	shape, err := playgroundShape(req.Body)
	if err != nil {
		return nil, err
	}
	if s.playground == nil {
		return nil, errNoPlayground
	}
	run, err := s.playground.Start(ctx, callerOf(ctx), shape)
	if ce, ok := errors.AsType[*demo.CapError](err); ok {
		after := ce.RetryAfter
		return apiv1.StartPlaygroundRun429JSONResponse{
			Body:    apiv1.Error{Error: ce.Msg, Code: apiv1.ErrorCodeUnavailable, RequestId: requestIDOf(ctx)},
			Headers: apiv1.StartPlaygroundRun429ResponseHeaders{RetryAfter: &after},
		}, nil
	}
	if errors.Is(err, demo.ErrNoConsumer) || errors.Is(err, demo.ErrGatewaysDown) {
		s.log.Warn("playground run refused", "sub", actorOf(ctx).Sub, "err", err, "request_id", requestIDOf(ctx))
		return nil, &apiError{http.StatusServiceUnavailable, apiv1.ErrorCodeUnavailable, err.Error(), nil}
	}
	if err != nil {
		return nil, err
	}
	in := run.Info()
	if err := s.store.RecordPlaygroundRun(ctx, audit(ctx, nil), "create", run.ID, map[string]any{"after": runDiff(in)}); err != nil {
		_, _ = s.playground.Stop(run.Sub, run.ID)
		return nil, err
	}
	return apiv1.StartPlaygroundRun201JSONResponse(runOut(in)), nil
}

// StopPlaygroundRun stops the caller's run and writes its audit record; a
// run that is not the caller's, or does not exist, is 404.
func (s *Server) StopPlaygroundRun(ctx context.Context, req apiv1.StopPlaygroundRunRequestObject) (apiv1.StopPlaygroundRunResponseObject, error) {
	if s.playground == nil {
		return nil, errNoRun
	}
	run, err := s.playground.Stop(actorOf(ctx).Sub, req.Id)
	if errors.Is(err, demo.ErrNoRun) {
		return nil, errNoRun
	}
	if err != nil {
		return nil, err
	}
	if err := s.store.RecordPlaygroundRun(ctx, audit(ctx, nil), "delete", run.ID, map[string]any{"before": runDiff(run.Info())}); err != nil {
		return nil, err
	}
	return apiv1.StopPlaygroundRun204Response{}, nil
}

var errNoRun = &apiError{http.StatusNotFound, apiv1.ErrorCodeNotFound, "no such run", nil}

// OpenPlaygroundStream implements the strict server interface; Handler
// routes the operation to openPlaygroundStream instead.
func (s *Server) OpenPlaygroundStream(context.Context, apiv1.OpenPlaygroundStreamRequestObject) (apiv1.OpenPlaygroundStreamResponseObject, error) {
	return nil, errors.New("openPlaygroundStream is served by its own handler")
}

// openPlaygroundStream streams the caller's run: 404 for a run that is
// not the caller's, 429 past the stream caps. It logs nothing but what
// the guard does.
func (s *Server) openPlaygroundStream(w http.ResponseWriter, r *http.Request) {
	if s.playground == nil {
		s.writeError(w, r, errNoRun)
		return
	}
	actor := actorOf(r.Context())
	sub, err := s.playground.Subscribe(actor.Sub, r.PathValue("id"))
	if ce, ok := errors.AsType[*demo.CapError](err); ok {
		w.Header().Set("Retry-After", strconv.Itoa(ce.RetryAfter))
		s.writeError(w, r, &apiError{http.StatusTooManyRequests, apiv1.ErrorCodeUnavailable, ce.Msg, nil})
		return
	}
	if err != nil {
		s.writeError(w, r, errNoRun)
		return
	}
	sub.Serve(w, r, actor.Expiry)
}
