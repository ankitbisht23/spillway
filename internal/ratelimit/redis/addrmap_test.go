package redis

import (
	"context"
	"net"
	"reflect"
	"testing"
)

func TestParseAddrMap(t *testing.T) {
	tests := []struct {
		in      string
		want    map[string]string
		wantErr bool
	}{
		{in: "", want: nil},
		{in: "   ", want: nil},
		{in: "redis-0:6379=127.0.0.1:7000", want: map[string]string{"redis-0:6379": "127.0.0.1:7000"}},
		{
			in:   " redis-0:6379 = 127.0.0.1:7000 , redis-1:6379=127.0.0.1:7001",
			want: map[string]string{"redis-0:6379": "127.0.0.1:7000", "redis-1:6379": "127.0.0.1:7001"},
		},
		{in: "redis-0:6379", wantErr: true},
		{in: "redis-0=127.0.0.1:7000", wantErr: true},
		{in: "redis-0:6379=127.0.0.1", wantErr: true},
		{in: "=127.0.0.1:7000", wantErr: true},
		{in: ":6379=127.0.0.1:7000", wantErr: true},
		{in: "redis-0:6379=127.0.0.1:7000,", wantErr: true},
		{in: "redis-0:6379=127.0.0.1:7000,redis-0:6379=127.0.0.1:7001", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseAddrMap(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseAddrMap(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseAddrMap(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestDialer(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	dial := Dialer(map[string]string{"redis-0:6379": ln.Addr().String()})
	for _, addr := range []string{
		"redis-0:6379",     // mapped: reaches the listener although the name doesn't resolve
		ln.Addr().String(), // not mapped: dialled as given
	} {
		c, err := dial(context.Background(), "tcp", addr)
		if err != nil {
			t.Fatalf("dial %s: %v", addr, err)
		}
		if got, want := c.RemoteAddr().String(), ln.Addr().String(); got != want {
			t.Errorf("dial %s reached %s, want %s", addr, got, want)
		}
		_ = c.Close()
	}
}

func FuzzParseAddrMap(f *testing.F) {
	f.Add("redis-0:6379=127.0.0.1:7000,redis-1:6379=127.0.0.1:7001")
	f.Add("a:1=b:2,")
	f.Add("=")
	f.Add("[::1]:6379=[::1]:7000")
	f.Fuzz(func(t *testing.T, s string) {
		m, err := ParseAddrMap(s)
		if err != nil {
			return
		}
		for from, to := range m {
			for _, addr := range []string{from, to} {
				if _, _, err := net.SplitHostPort(addr); err != nil {
					t.Fatalf("ParseAddrMap(%q) accepted %q: %v", s, addr, err)
				}
			}
		}
	})
}
