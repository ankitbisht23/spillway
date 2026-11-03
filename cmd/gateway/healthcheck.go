package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// healthcheck asks the admin port's /readyz and fails unless it answers 200.
// It backs `gateway healthcheck`, the compose healthcheck: the image has no
// shell or HTTP client. A wildcard admin address is asked on 127.0.0.1.
func healthcheck(ctx context.Context, getenv func(string) string) error {
	host, port, err := net.SplitHostPort(adminAddr(getenv))
	if err != nil {
		return fmt.Errorf("SPILLWAY_GATEWAY_ADMIN_ADDR: %w", err)
	}
	if host == "" || net.ParseIP(host).IsUnspecified() {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz: %s", resp.Status)
	}
	return nil
}
