package stream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/encoding/proto"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// encoded is a message marshalled once, which the codec sends as it is to
// every stream (ADR-0038).
type encoded []byte

// codec is gRPC's proto codec, except that it sends an encoded as it is.
type codec struct{ encoding.CodecV2 }

func (c codec) Marshal(v any) (mem.BufferSlice, error) {
	if e, ok := v.(encoded); ok {
		return mem.BufferSlice{mem.SliceBuffer(e)}, nil
	}
	return c.CodecV2.Marshal(v)
}

func currentSchema() *configpb.SchemaVersion {
	return &configpb.SchemaVersion{Major: config.SchemaMajor, Minor: config.SchemaMinor}
}

// acceptSchema reports whether a gateway built with schema v may watch
// (CP-18, ADR-0038): the same major, and a minor at most one behind; newer
// minors are accepted too.
func acceptSchema(v *configpb.SchemaVersion) bool {
	return v.GetMajor() == config.SchemaMajor && v.GetMinor() >= config.SchemaMinor-1
}

// replicaName is a DNS label, as a client certificate's name must be
// (ADR-0040).
var replicaName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Service is ConfigService on the active replica (CP-06). Heartbeat
// arrives with M5's heartbeat task (ADR-0045); until then it is
// UNIMPLEMENTED.
type Service struct {
	configpb.UnimplementedConfigServiceServer
	b   *Broadcaster
	log *slog.Logger
}

// NewService serves the broadcaster's updates.
func NewService(b *Broadcaster, log *slog.Logger) *Service { return &Service{b: b, log: log} }

// Watch registers the stream under its replica name, sends the current
// config and then the current identity when their versions differ from the
// request's, then each new version, until the stream ends, another stream
// with the same name replaces it (ABORTED), or the replica steps down
// (UNAVAILABLE) (CP-06, ADR-0038).
func (s *Service) Watch(req *configpb.WatchRequest, stream grpc.ServerStreamingServer[configpb.WatchResponse]) error {
	// Until M5's mtls-secrets task the name is the request's; then it is
	// the client certificate's, which the request must match (ADR-0040).
	name := req.GetReplicaId()
	if !replicaName.MatchString(name) {
		return status.Error(codes.InvalidArgument, "replica_id: want a DNS label, such as gateway-1")
	}
	log := s.log.With("replica", name)
	if !acceptSchema(req.GetSchemaVersion()) {
		v := req.GetSchemaVersion()
		log.Warn("Watch refused: schema version not served", "schema", fmt.Sprintf("%d.%d", v.GetMajor(), v.GetMinor()))
		return status.Errorf(codes.FailedPrecondition, "schema %d.%d is not served: this control plane serves %d.%d and one minor version behind (CP-18)",
			v.GetMajor(), v.GetMinor(), config.SchemaMajor, config.SchemaMinor)
	}
	ctx := stream.Context()
	select {
	case <-s.b.Loaded():
	case <-ctx.Done():
		return ctx.Err()
	}
	sub, replaced, err := s.b.subscribe(name)
	if err != nil {
		return err
	}
	defer s.b.unsubscribe(sub)
	// The headers tell the gateway that its stream is registered.
	if err := stream.SendHeader(nil); err != nil {
		return err
	}
	if replaced {
		log.Warn("Watch stream replaced an open stream with the same replica name")
	}
	log.Info("Watch stream opened", "config_version", req.GetConfigVersion(), "identity_version", req.GetIdentityVersion())
	sent := [sections]int64{req.GetConfigVersion(), req.GetIdentityVersion()}
	for {
		for sec := range sections {
			if u := s.b.current(sec); u != nil && u.version != sent[sec] {
				if err := stream.SendMsg(u.msg); err != nil {
					return err
				}
				sent[sec] = u.version
			}
		}
		select {
		case <-sub.wake:
		case <-sub.done:
			log.Info("Watch stream closed", "reason", status.Convert(sub.err).Message())
			return sub.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// gRPC limits and keepalives (ADR-0038).
const (
	maxMessage = 64 << 20
	// The server pings an idle client after 10 s and drops it after 5 s
	// more; clients may ping every 5 s.
	keepaliveTime    = 10 * time.Second
	keepaliveTimeout = 5 * time.Second
	keepaliveMinPing = 5 * time.Second
	// stopGrace bounds the graceful stop of the gRPC server.
	stopGrace = 5 * time.Second
)

// serverOptions are the ConfigService server's options, then extra (M5's
// mtls-secrets task adds transport credentials there).
func serverOptions(extra ...grpc.ServerOption) []grpc.ServerOption {
	return append([]grpc.ServerOption{
		grpc.ForceServerCodecV2(codec{encoding.GetCodecV2(proto.Name)}),
		grpc.MaxRecvMsgSize(maxMessage),
		grpc.MaxSendMsgSize(maxMessage),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: keepaliveTime, Timeout: keepaliveTimeout}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: keepaliveMinPing, PermitWithoutStream: true}),
	}, extra...)
}

// Serve is what the active replica runs (CP-22, ADR-0038): the broadcaster
// and, once it has read both sections, ConfigService on a listener at addr.
// When ctx ends it closes every stream with UNAVAILABLE, stops the
// broadcaster and closes the listener. It returns early only if it cannot
// listen.
func Serve(ctx context.Context, addr string, src Source, log *slog.Logger, streams func(int), opts ...grpc.ServerOption) error {
	bctx, stopB := context.WithCancel(context.Background())
	b := NewBroadcaster(src, log, streams)
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		b.Run(bctx)
	}()
	defer func() {
		stopB()
		<-bDone
	}()
	select {
	case <-b.Loaded():
	case <-ctx.Done():
		return nil
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("gRPC listener: %w", err)
	}
	gs := grpc.NewServer(serverOptions(opts...)...)
	configpb.RegisterConfigServiceServer(gs, NewService(b, log))
	served := make(chan error, 1)
	go func() { served <- gs.Serve(ln) }()
	log.Info("listening", "server", "grpc", "addr", ln.Addr().String())

	select {
	case <-ctx.Done():
	case err = <-served:
		if errors.Is(err, grpc.ErrServerStopped) {
			err = nil
		}
	}
	// Streams end with UNAVAILABLE, then the listener closes.
	stopB()
	<-bDone
	stopped := make(chan struct{})
	go func() {
		gs.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(stopGrace):
		gs.Stop()
	}
	log.Info("gRPC listener closed")
	return err
}
