package sandbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// A small Kubernetes client instead of client-go. It polls instead of watching
// and sends merge patches instead of applying.

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// tokenRefresh is how often the token is re-read. The kubelet rotates it, so
// a token read once fails after about an hour.
const tokenRefresh = 5 * time.Minute

type kubeClient struct {
	base string
	http *http.Client

	// tokenPath is empty for a test's static token.
	tokenPath string
	mu        sync.RWMutex
	token     string
	tokenRead time.Time
}

// KubeConfig overrides in-cluster defaults. Only tests set it: the gateway
// always runs in the cluster, because it dials a sandbox's pod IP.
type KubeConfig struct {
	Server   string
	Token    string
	Insecure bool
}

func newKubeClient(cfg KubeConfig) (*kubeClient, error) {
	c := &kubeClient{base: strings.TrimRight(cfg.Server, "/")}

	if c.base == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return nil, errors.New("not running in a cluster: KUBERNETES_SERVICE_HOST is unset; " +
				"the kubernetes driver runs in the cluster it creates sandboxes in")
		}
		c.base = "https://" + net.JoinHostPort(host, port)
	}

	if cfg.Token != "" {
		c.token = cfg.Token
	} else {
		c.tokenPath = serviceAccountDir + "/token"
		if err := c.readToken(); err != nil {
			return nil, fmt.Errorf("reading the service account token: %w", err)
		}
	}

	tlsCfg, err := kubeTLS(cfg)
	if err != nil {
		return nil, err
	}
	c.http = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     tlsCfg,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	return c, nil
}

func kubeTLS(cfg KubeConfig) (*tls.Config, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.Insecure} //nolint:gosec // only tests set cfg.Insecure
	if cfg.Insecure {
		return tlsCfg, nil
	}
	caFile := serviceAccountDir + "/ca.crt"
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading the cluster CA bundle %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no certificates", caFile)
	}
	tlsCfg.RootCAs = pool
	return tlsCfg, nil
}

// inClusterNamespace is this pod's namespace, or "default" outside a cluster.
func inClusterNamespace() string {
	if raw, err := os.ReadFile(serviceAccountDir + "/namespace"); err == nil {
		if ns := strings.TrimSpace(string(raw)); ns != "" {
			return ns
		}
	}
	return "default"
}

func (c *kubeClient) readToken() error {
	raw, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token, c.tokenRead = strings.TrimSpace(string(raw)), time.Now()
	return nil
}

// bearer re-reads a rotated token. If that fails it keeps the old one, which
// is still valid for a while.
func (c *kubeClient) bearer() string {
	c.mu.RLock()
	tok, read := c.token, c.tokenRead
	c.mu.RUnlock()
	if c.tokenPath == "" || time.Since(read) < tokenRefresh {
		return tok
	}
	if err := c.readToken(); err != nil {
		return tok
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

type kubeError struct {
	Status int
	Msg    string
	Path   string
}

func (e *kubeError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("kubernetes: %s (%d)", e.Msg, e.Status)
	}
	return fmt.Sprintf("kubernetes: %s returned %d", e.Path, e.Status)
}

// isNotFound and isConflict are the refusals the driver acts on.
func isNotFound(err error) bool { return hasKubeStatus(err, http.StatusNotFound) }
func isConflict(err error) bool { return hasKubeStatus(err, http.StatusConflict) }

func hasKubeStatus(err error, status int) bool {
	var ke *kubeError
	return errors.As(err, &ke) && ke.Status == status
}

// kubeNotFound maps a 404 to ErrNotFound.
func kubeNotFound(err error) error {
	if isNotFound(err) {
		return ErrNotFound
	}
	return err
}

// maxKubeBody keeps a misrouted request from filling memory.
const maxKubeBody = 8 << 20

func (c *kubeClient) do(ctx context.Context, method, path, contentType string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if tok := c.bearer(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reaching the Kubernetes API at %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxKubeBody))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var st struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &st)
		return &kubeError{Status: resp.StatusCode, Msg: st.Message, Path: path}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (c *kubeClient) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, "", nil, out)
}

func (c *kubeClient) post(ctx context.Context, path string, in, out any) error {
	return c.do(ctx, http.MethodPost, path, "", in, out)
}

// patch sends a JSON merge patch, which needs no schema. It replaces lists, so
// a list must be given whole.
func (c *kubeClient) patch(ctx context.Context, path string, in any) error {
	return c.do(ctx, http.MethodPatch, path, "application/merge-patch+json", in, nil)
}

// delete uses foreground propagation, so a terminated sandbox's pod is already
// going.
func (c *kubeClient) delete(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, path, "",
		map[string]any{
			"apiVersion":        "meta.k8s.io/v1",
			"kind":              "DeleteOptions",
			"propagationPolicy": "Foreground",
		}, nil)
}
