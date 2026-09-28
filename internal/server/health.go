package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// health asks the server listening on addr whether it is ready. It exists
// because the image is FROM scratch: a container health check has no shell or
// curl, only this binary.
func health(ctx context.Context, addr string) error {
	url, err := readyURL(addr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return nil
}

// readyURL is /readyz on the listener addr names. A listener on every
// interface is reached on loopback.
func readyURL(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("KEERA_ADDR %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/readyz", nil
}
