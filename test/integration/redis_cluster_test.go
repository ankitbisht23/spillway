//go:build integration

// Package integration holds tests that need the compose stack; run them with
// make test-integration.
package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// TestRedisCluster checks the compose Redis Cluster from the host through the
// address map (ADR-0004): the cluster is healthy with 3 primaries and 3
// replicas, and a write lands on every primary. Lines starting "measure:" are
// printed by make verify-m0.
func TestRedisCluster(t *testing.T) {
	addrMap, err := redis.ParseAddrMap(os.Getenv(redis.AddrMapEnv))
	if err != nil {
		t.Fatal(err)
	}
	if addrMap == nil {
		t.Fatalf("%s is not set; run make test-integration", redis.AddrMapEnv)
	}
	dial := redis.Dialer(addrMap)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: []string{"redis-0:6379"}, Dialer: dial})
	defer func() { _ = c.Close() }()

	info, err := c.ClusterInfo(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	state, slots := infoField(info, "cluster_state"), infoField(info, "cluster_slots_assigned")
	t.Logf("measure: cluster_state=%s slots_assigned=%s", state, slots)
	if state != "ok" || slots != "16384" {
		t.Fatalf("cluster_state=%s slots_assigned=%s, want ok and 16384", state, slots)
	}

	shards, err := c.ClusterShards(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	primaries, replicas := 0, 0
	for _, s := range shards {
		for _, n := range s.Nodes {
			switch n.Role {
			case "master":
				primaries++
			case "replica":
				replicas++
			}
		}
	}
	t.Logf("measure: primaries=%d replicas=%d", primaries, replicas)
	if len(shards) != 3 || primaries != 3 || replicas != 3 {
		t.Fatalf("%d shards, %d primaries, %d replicas; want 3, 3, 3", len(shards), primaries, replicas)
	}

	// Write one key per primary through the cluster client, then read it back
	// from that primary alone: a key living elsewhere would answer MOVED.
	done := make(map[int]bool)
	written := 0
	for i := 0; written < len(shards) && i < 1000; i++ {
		key := fmt.Sprintf("m0:smoke:%d", i)
		slot, err := c.ClusterKeySlot(ctx, key).Result()
		if err != nil {
			t.Fatal(err)
		}
		j := shardOf(shards, slot)
		if j < 0 || done[j] {
			continue
		}
		done[j] = true
		primary := primaryAddr(shards[j])
		if err := c.Set(ctx, key, key, time.Minute).Err(); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
		node := goredis.NewClient(&goredis.Options{Addr: primary, Dialer: dial})
		got, err := node.Get(ctx, key).Result()
		_ = node.Close()
		if err != nil || got != key {
			t.Fatalf("get %s from %s = %q, %v; want %q", key, primary, got, err, key)
		}
		_ = c.Del(ctx, key).Err()
		written++
	}
	t.Logf("measure: smoke write from the host landed on %d/3 primaries", written)
	if written != 3 {
		t.Fatalf("wrote to %d primaries, want 3", written)
	}
}

func infoField(info, name string) string {
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), name+":"); ok {
			return v
		}
	}
	return ""
}

func primaryAddr(s goredis.ClusterShard) string {
	for _, n := range s.Nodes {
		if n.Role == "master" {
			return fmt.Sprintf("%s:%d", n.Endpoint, n.Port)
		}
	}
	return ""
}

// shardOf returns the index of the shard that owns slot, or -1.
func shardOf(shards []goredis.ClusterShard, slot int64) int {
	for i, s := range shards {
		for _, r := range s.Slots {
			if slot >= r.Start && slot <= r.End {
				return i
			}
		}
	}
	return -1
}
