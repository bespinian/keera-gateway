package policy

import "testing"

func TestHostingTellsWhatLeavesTheNetworkApartFromWhatDoesNot(t *testing.T) {
	tests := []struct {
		name     string
		backends []string
		want     Hosting
	}{
		{"a service name in a cluster", []string{"http://vllm:8000/v1"}, HostedInternal},
		{"a Kubernetes service", []string{"http://vllm.inference.svc.cluster.local/v1"}, HostedInternal},
		{"the loopback", []string{"http://127.0.0.1:11434/v1"}, HostedInternal},
		{"a private address", []string{"https://10.4.2.9/v1"}, HostedInternal},
		{"a machine on the LAN", []string{"http://gpu-box.local:8000/v1"}, HostedInternal},
		{"a hosted provider", []string{"https://api.anthropic.com/v1"}, HostedExternal},
		{"a public address", []string{"https://203.0.113.7/v1"}, HostedExternal},
		{"an entry written as a bare host", []string{"vllm.internal:8000"}, HostedInternal},
		{"a model with no backend at all", nil, HostedUnknown},
		{"something that is not a URL", []string{"::not a url"}, HostedUnknown},
		{
			// The gateway forwards to whichever it picks, so the promise is
			// only as good as the weakest entry.
			name:     "one external backend among internal ones",
			backends: []string{"http://vllm:8000/v1", "https://api.anthropic.com/v1"},
			want:     HostedExternal,
		},
		{
			name:     "an unreadable backend is not quietly called safe",
			backends: []string{"http://vllm:8000/v1", "?"},
			want:     HostedUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Model{Backends: tc.backends}).Hosting(); got != tc.want {
				t.Errorf("Hosting(%v) = %q, want %q", tc.backends, got, tc.want)
			}
		})
	}
}

func TestEndpointNamesTheHostThatDecidedTheClassification(t *testing.T) {
	// The host on the screen has to be the reason for the mark beside it: a
	// model drawn outside the boundary that names its internal backend would
	// be a screen nobody can check.
	m := Model{Backends: []string{"http://vllm:8000/v1", "https://api.anthropic.com/v1"}}
	if got := m.Endpoint(); got != "api.anthropic.com" {
		t.Errorf("Endpoint() = %q, want the external backend", got)
	}
	if got := (Model{Backends: []string{"http://vllm:8000/v1"}}).Endpoint(); got != "vllm:8000" {
		t.Errorf("Endpoint() = %q, want host and port and nothing else", got)
	}
	// A credential in a backend URL is not a thing to put on a screen.
	secret := Model{Backends: []string{"https://user:pw@api.example.com/v1"}}
	if got := secret.Endpoint(); got != "api.example.com" {
		t.Errorf("Endpoint() = %q, which carries the URL's credentials", got)
	}
}
