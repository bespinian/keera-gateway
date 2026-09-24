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

// A small Kubernetes client instead of client-go (see the package comment).
//
// It has no watch, no field managers, no server-side apply and no discovery.
// The driver polls instead of watching and sends merge patches instead of
// applying.

// serviceAccountDir is where the kubelet mounts a pod's identity.
const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// tokenRefresh is how often the service account token is re-read. The kubelet
// rotates the file, so a token read only once starts failing after about an
// hour.
const tokenRefresh = 5 * time.Minute

// kubeClient talks to the Kubernetes API server.
type kubeClient struct {
	base string
	http *http.Client

	// tokenPath is re-read on a timer. Empty for a static token, as in tests
	// and outside a cluster.
	tokenPath string
	mu        sync.RWMutex
	token     string
	tokenRead time.Time
}

// KubeConfig points the client at an API server. Every field overrides an
// in-cluster default.
type KubeConfig struct {
	// Server is the API server's base URL. Empty means in-cluster, from
	// KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT.
	Server string
	// Token and TokenPath are the credential. In a cluster use the path,
	// because the token rotates; a literal token is for development outside
	// one.
	Token     string
	TokenPath string
	// CAFile verifies the API server. Empty means the in-cluster bundle.
	CAFile string
	// Insecure skips certificate checks. It is only for development clusters,
	// must be set by name, and is logged loudly.
	Insecure bool
}

// newKubeClient builds a client, using the in-cluster configuration for
// anything left blank.
func newKubeClient(cfg KubeConfig) (*kubeClient, error) {
	c := &kubeClient{base: strings.TrimRight(cfg.Server, "/")}

	if c.base == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return nil, errors.New("not running in a cluster: KUBERNETES_SERVICE_HOST is unset, " +
				"and no API server was configured (KEERA_SANDBOX_KUBE_SERVER)")
		}
		c.base = "https://" + net.JoinHostPort(host, port)
	}

	switch {
	case cfg.Token != "":
		c.token = cfg.Token
	case cfg.TokenPath != "":
		c.tokenPath = cfg.TokenPath
	default:
		c.tokenPath = serviceAccountDir + "/token"
	}
	if c.tokenPath != "" {
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

// kubeTLS is the TLS configuration for the API server.
func kubeTLS(cfg KubeConfig) (*tls.Config, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.Insecure} //nolint:gosec // guarded by cfg.Insecure, which a deployment sets by name
	if cfg.Insecure {
		return tlsCfg, nil
	}
	caFile := cfg.CAFile
	if caFile == "" {
		caFile = serviceAccountDir + "/ca.crt"
	}
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

// inClusterNamespace is the namespace this pod runs in, or "default" outside a
// cluster. It is only a fallback for an unset namespace setting.
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

// bearer returns the current token, re-reading a rotated one.
//
// If the re-read fails it keeps the old token: that one is still valid for a
// while, and the kubelet may be halfway through replacing the file.
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

// kubeStatus is the error body the API server returns.
type kubeStatus struct {
	Message string `json:"message"`
}

// kubeError is a refusal from the API server.
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

// isNotFound and isConflict are the two refusals the driver acts on.
func isNotFound(err error) bool { return hasKubeStatus(err, http.StatusNotFound) }
func isConflict(err error) bool { return hasKubeStatus(err, http.StatusConflict) }

func hasKubeStatus(err error, status int) bool {
	var ke *kubeError
	return errors.As(err, &ke) && ke.Status == status
}

// kubeNotFound turns a 404 into ErrNotFound and passes anything else through.
func kubeNotFound(err error) error {
	if isNotFound(err) {
		return ErrNotFound
	}
	return err
}

// maxKubeBody bounds what is read back from the API server, so a misrouted
// request cannot fill memory.
const maxKubeBody = 8 << 20

// do makes one request. contentType is only used when there is a body, and
// defaults to JSON.
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
		var st kubeStatus
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

// patch sends a JSON merge patch, which needs no schema and no field manager.
// A merge patch replaces lists rather than merging them, so nothing here
// patches a list.
func (c *kubeClient) patch(ctx context.Context, path string, in, out any) error {
	return c.do(ctx, http.MethodPatch, path, "application/merge-patch+json", in, out)
}

// delete uses foreground propagation, so a sandbox reported as terminated
// already has its pod on the way out.
func (c *kubeClient) delete(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, path, "application/json",
		map[string]any{
			"apiVersion":        "meta.k8s.io/v1",
			"kind":              "DeleteOptions",
			"propagationPolicy": "Foreground",
		}, nil)
}
