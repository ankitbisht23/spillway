package stream

import (
	"context"
	"log/slog"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/ankitbisht23/spillway/internal/mtls"
)

// The stream's peers (SEC-02, D7, ADR-0040): every connection is mTLS
// with a gateway certificate of the stream CA, and the replica name is the
// certificate's.

// loggedCreds logs the handshakes the server refuses (no client
// certificate, another CA, expired, no client-auth usage) with the
// peer's address and crypto/tls's reason, which holds no key material;
// gRPC itself would drop them silently.
type loggedCreds struct {
	credentials.TransportCredentials
	log *slog.Logger
}

func (c loggedCreds) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	out, info, err := c.TransportCredentials.ServerHandshake(conn)
	if err != nil {
		c.log.Warn("config stream: TLS handshake refused", "remote", conn.RemoteAddr().String(), "err", err)
	}
	return out, info, err
}

func (c loggedCreds) Clone() credentials.TransportCredentials {
	return loggedCreds{c.TransportCredentials.Clone(), c.log}
}

// replicaKey keys the replica name in a call's context.
type replicaKey struct{}

// withReplica takes the replica name from the verified client certificate
// into ctx; a certificate that does not name a replica is PERMISSION_DENIED
// and logged.
func withReplica(ctx context.Context, log *slog.Logger) (context.Context, error) {
	p, _ := peer.FromContext(ctx)
	var info credentials.TLSInfo
	if p != nil {
		info, _ = p.AuthInfo.(credentials.TLSInfo)
	}
	if len(info.State.VerifiedChains) == 0 {
		return ctx, status.Error(codes.Unauthenticated, "a client certificate of the stream CA is required")
	}
	leaf := info.State.VerifiedChains[0][0]
	name, err := mtls.ReplicaName(leaf)
	if err != nil {
		log.Warn("config stream: client certificate refused", "remote", p.Addr.String(), "serial", leaf.SerialNumber.String(),
			"dns_names", leaf.DNSNames, "err", err)
		return ctx, status.Error(codes.PermissionDenied, err.Error())
	}
	return context.WithValue(ctx, replicaKey{}, name), nil
}

func unaryReplica(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := withReplica(ctx, log)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func streamReplica(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := withReplica(ss.Context(), log)
		if err != nil {
			return err
		}
		return handler(srv, replicaStream{ss, ctx})
	}
}

// replicaStream is a ServerStream whose context carries the replica name.
type replicaStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s replicaStream) Context() context.Context { return s.ctx }

// maxLoggedID caps how much of a request's replica_id is logged.
const maxLoggedID = 64

// replica is the call's replica name, from its client certificate. A
// request's replica_id must be empty or equal to it; a differing one is
// PERMISSION_DENIED and logged with both names (ADR-0038, ADR-0040).
func (s *Service) replica(ctx context.Context, method, requested string) (string, error) {
	name, _ := ctx.Value(replicaKey{}).(string)
	if name == "" {
		return "", status.Error(codes.Unauthenticated, "a client certificate of the stream CA is required")
	}
	if requested != "" && requested != name {
		s.log.Warn("config stream: replica_id differs from the client certificate's name", "method", method,
			"replica", name, "replica_id", requested[:min(len(requested), maxLoggedID)])
		return "", status.Errorf(codes.PermissionDenied, "replica_id %q is not the client certificate's name %q", requested[:min(len(requested), maxLoggedID)], name)
	}
	return name, nil
}
