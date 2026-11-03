package main

import (
	"strings"
	"testing"
	"time"
)

func TestLoadSettings(t *testing.T) {
	base := map[string]string{
		"SPILLWAY_GATEWAY_CONFIG":           "/etc/spillway/config.yaml",
		"SPILLWAY_GATEWAY_HTTP_ADDR":        ":8000",
		"SPILLWAY_GATEWAY_ALLOWED_NETWORKS": "172.29.0.0/16",
	}
	env := func(over map[string]string) func(string) string {
		return func(k string) string {
			if v, ok := over[k]; ok {
				return v
			}
			return base[k]
		}
	}

	s, err := loadSettings(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if s.configPath != "/etc/spillway/config.yaml" || s.httpAddr != ":8000" || len(s.allow) != 1 || s.dnsRefresh != 30*time.Second {
		t.Errorf("settings = %+v", s)
	}
	if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_DNS_REFRESH": "5s"})); err != nil || s.dnsRefresh != 5*time.Second {
		t.Errorf("DNS refresh 5s: %+v, %v", s, err)
	}

	for name, over := range map[string]map[string]string{
		"SPILLWAY_GATEWAY_CONFIG":           {"SPILLWAY_GATEWAY_CONFIG": ""},
		"SPILLWAY_GATEWAY_HTTP_ADDR":        {"SPILLWAY_GATEWAY_HTTP_ADDR": ""},
		"SPILLWAY_GATEWAY_ALLOWED_NETWORKS": {"SPILLWAY_GATEWAY_ALLOWED_NETWORKS": ""},
		"SPILLWAY_GATEWAY_DNS_REFRESH":      {"SPILLWAY_GATEWAY_DNS_REFRESH": "0s"},
	} {
		if _, err := loadSettings(env(over)); err == nil || !strings.HasPrefix(err.Error(), name) {
			t.Errorf("%v: error = %v, want one naming %s", over, err, name)
		}
	}
}

func TestParseMemoryMax(t *testing.T) {
	for in, want := range map[string]int64{
		"536870912\n": 536870912,
		"max\n":       0,
		"":            0,
		"0\n":         0,
		"-1\n":        0,
	} {
		got, ok := parseMemoryMax([]byte(in))
		if ok != (want > 0) || ok && got != want {
			t.Errorf("parseMemoryMax(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
}
