package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/live"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// The live-data stream, GET /stream (UI-03, RL-21, ADR-0059): guarded as
// every operation, then served by its own handler, because the strict
// server answers with one response object and cannot flush per event.

// openStreamID is the operation the stream handler serves.
const openStreamID = "openStream"

// WithStreams makes s serve GET /stream from hub and returns s. Without
// it, the stream answers 503.
func (s *Server) WithStreams(hub *live.Hub) *Server {
	s.streams = hub
	return s
}

// OpenStream implements the strict server interface; Handler routes the
// operation to openStream instead, so it is never called.
func (s *Server) OpenStream(context.Context, apiv1.OpenStreamRequestObject) (apiv1.OpenStreamResponseObject, error) {
	return nil, errors.New("openStream is served by its own handler")
}

// openStream validates the request (400 before any byte of stream),
// admits it against the limits (503) and streams until it ends. It logs
// nothing of the request but what the guard does: the sub, never the
// token.
func (s *Server) openStream(w http.ResponseWriter, r *http.Request) {
	if s.streams == nil {
		s.writeError(w, r, &apiError{http.StatusServiceUnavailable, apiv1.ErrorCodeUnavailable, "the live-data stream is not available", nil})
		return
	}
	actor := actorOf(r.Context())
	st, err := s.streams.Open(r.Context(), actor.Sub, r.URL.Query())
	if inv, ok := errors.AsType[*live.InvalidError](err); ok {
		s.writeError(w, r, invalid(inv.Msg))
		return
	}
	if lim, ok := errors.AsType[*live.LimitError](err); ok {
		w.Header().Set("Retry-After", live.RetryAfter)
		s.writeError(w, r, &apiError{http.StatusServiceUnavailable, apiv1.ErrorCodeUnavailable, lim.Msg, nil})
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	st.Serve(w, r, actor.Expiry)
}

// LiveSource is what the stream reads from the store, in the REST
// answers' shapes (ADR-0059).
func (s *Server) LiveSource() live.Postgres { return liveSource{s.store} }

type liveSource struct{ st *store.Store }

// heavyHitterLimit is how many keys throttling.heavy_hitters shows.
const heavyHitterLimit = 50

// RouteExists looks in the latest published config, which the gateways
// run; before the first publish there is no route.
func (l liveSource) RouteExists(ctx context.Context, name string) (bool, error) {
	latest, err := l.st.LatestConfig(ctx)
	if err != nil {
		return false, err
	}
	for _, r := range latest.GetConfig().GetRoutes() {
		if r.GetName() == name {
			return true, nil
		}
	}
	return false, nil
}

// TenantExists looks the name up by the tenants' unique name index.
func (l liveSource) TenantExists(ctx context.Context, name string) (bool, error) {
	rows, err := l.st.ListTenants(ctx, store.Page{Limit: 1}, &name)
	return len(rows) > 0, err
}

// streamReplicas and streamReplica are the spec's StreamReplicas and
// StreamReplica.
type streamReplicas struct {
	Items []streamReplica `json:"items"`
}

type streamReplica struct {
	Replica    apiv1.Replica `json:"replica"`
	StalenessS *float64      `json:"staleness_s"`
}

func (l liveSource) Replicas(ctx context.Context) (any, error) {
	rows, err := l.st.AllReplicas(ctx)
	if err != nil {
		return nil, err
	}
	out := streamReplicas{Items: make([]streamReplica, len(rows))}
	for i, r := range rows {
		out.Items[i].Replica = replicaOf(r.Replica)
		if r.Staleness != nil {
			sec := max(r.Staleness.Seconds(), 0)
			out.Items[i].StalenessS = &sec
		}
	}
	return out, nil
}

func (l liveSource) HeavyHitters(ctx context.Context, since time.Time, policyID *int64) (any, error) {
	rows, err := l.st.TopHeavyHitters(ctx, store.HeavyHitterFilter{Since: &since, PolicyID: policyID}, heavyHitterLimit)
	if err != nil {
		return nil, err
	}
	return heavyHittersOf(rows), nil
}

func (liveSource) Unavailable(err error) bool { return store.Unavailable(err) }
