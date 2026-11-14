//go:build integration

package integration

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// The gateway's Redis AUTH (SEC-09, ADR-0040). The compose Redis Cluster
// has no password (ADR-0035 P11), so this runs the limiter against a
// one-node cluster in a testcontainer with requirepass and an ACL user.

const (
	redisAuthImage    = "redis:8.10.3"              // compose's image
	redisAuthPassword = "redis-dev-only-not-secret" //nolint:gosec // G101: dev-only
	redisACLPassword  = "acl-dev-only-not-secret"   //nolint:gosec // G101: dev-only
	redisWrongPass    = "wrong-dev-only-not-secret" //nolint:gosec // G101: dev-only
	redisAuthHost     = "redis-auth:6379"           // the node announces this name; the address map sends it to the mapped port
)

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// redisWithAuth starts the one-node cluster and returns the address map
// that reaches it.
func redisWithAuth(t *testing.T) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        redisAuthImage,
			ExposedPorts: []string{"6379/tcp"},
			Cmd: []string{"redis-server", "--port", "6379", "--cluster-enabled", "yes", "--appendonly", "no",
				"--requirepass", redisAuthPassword, "--masterauth", redisAuthPassword,
				"--cluster-announce-hostname", "redis-auth", "--cluster-preferred-endpoint-type", "hostname"},
			WaitingFor: wait.ForLog("Ready to accept connections"),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatal(err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := c.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatal(err)
	}
	addr := host + ":" + port.Port()
	admin := goredis.NewClient(&goredis.Options{Addr: addr, Password: redisAuthPassword})
	defer func() { _ = admin.Close() }()
	if err := admin.Do(ctx, "CLUSTER", "ADDSLOTSRANGE", "0", "16383").Err(); err != nil {
		t.Fatal(err)
	}
	if err := admin.Do(ctx, "ACL", "SETUSER", "spillway", "on", ">"+redisACLPassword, "~*", "&*", "+@all").Err(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "cluster_state:ok", func() bool {
		info, err := admin.ClusterInfo(ctx).Result()
		return err == nil && strings.Contains(info, "cluster_state:ok")
	})
	return map[string]string{redisAuthHost: addr}
}

// TestRedisAuth checks the gateway's Redis AUTH settings against a Redis
// with requirepass: the default user's password and an ACL user's
// username and password warm the limiter and decide calls; a wrong
// password or none is refused; no log line shows a password.
func TestRedisAuth(t *testing.T) {
	addrMap := redisWithAuth(t)
	var logs logBuffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	call := func(auth redis.Auth) error {
		t.Helper()
		c := redis.NewClient([]string{redisAuthHost}, addrMap, auth, ratelimit.SystemClock, log)
		defer func() { _ = c.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		key := redis.GlobalKey(scriptName("auth"), time.Minute, redis.Digest("k"))
		_, err := c.Call(ctx, 1, []redis.Limit{gcraLimit(key, "global", true, 10, time.Minute, 10)})
		return err
	}
	warm := func(auth redis.Auth) bool {
		t.Helper()
		c := redis.NewClient([]string{redisAuthHost}, addrMap, auth, ratelimit.SystemClock, log)
		defer func() { _ = c.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c.Warm(ctx, 100*time.Millisecond)
		return ctx.Err() == nil // Warm returned on success
	}

	for name, auth := range map[string]redis.Auth{
		"default user": {Password: secret.New(redisAuthPassword)},
		"ACL user":     {Username: "spillway", Password: secret.New(redisACLPassword)},
	} {
		if !warm(auth) {
			t.Errorf("%s: the warm-up did not succeed", name)
		}
		eventually(t, name+": a call decided by Redis", func() bool { return call(auth) == nil })
	}
	for name, auth := range map[string]redis.Auth{
		"wrong password":    {Password: secret.New(redisWrongPass)},
		"ACL user, default": {Username: "spillway", Password: secret.New(redisAuthPassword)},
		"no password":       {},
	} {
		if warm(auth) {
			t.Errorf("%s: warmed", name)
		}
		if err := call(auth); err == nil {
			t.Errorf("%s: a call succeeded", name)
		}
	}
	out := logs.String()
	if !strings.Contains(out, "redis limiter warm-up failed") {
		t.Errorf("the refused warm-ups were not logged:\n%s", out)
	}
	for _, pw := range []string{redisAuthPassword, redisACLPassword, redisWrongPass} {
		if strings.Contains(out, pw) {
			t.Errorf("a log line shows a password:\n%s", out)
		}
	}
}
