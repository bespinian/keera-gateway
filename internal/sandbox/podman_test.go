package sandbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPodmanKeepsSecretsOffItsCommandLine(t *testing.T) {
	// Every user of the host can read a process's arguments, so a key or a
	// token there would be anybody's.
	t.Setenv("PATH", "/usr/bin")
	p := &Podman{}
	spec := Spec{Env: map[string]string{
		"KEERA_API_KEY": "keera_sk_secret", "KEERA_TASK": "fix the login",
		"GITHUB_TOKEN": "ghp_secret", "PATH": "/opt/bin:/usr/bin",
	}}
	args, env := p.runArgs(spec, "")
	line := strings.Join(args, " ")
	for _, secret := range []string{"keera_sk_secret", "ghp_secret", "fix the login"} {
		if strings.Contains(line, secret) {
			t.Errorf("the command line holds %q: %s", secret, line)
		}
	}
	if !slices.Contains(args, "KEERA_API_KEY") || !slices.Contains(env, "KEERA_API_KEY=keera_sk_secret") {
		t.Errorf("the key is not handed to podman through its environment: %v / %v", args, env)
	}
	if !slices.Contains(env, "GITHUB_TOKEN=ghp_secret") {
		t.Errorf("a caller's own value is not handed to podman through its environment: %v", env)
	}
	// podman would otherwise run with the sandbox's PATH.
	if !slices.Contains(args, "PATH=/opt/bin:/usr/bin") || slices.Contains(env, "PATH=/opt/bin:/usr/bin") {
		t.Errorf("a name podman has should stay on the command line: %v / %v", args, env)
	}
}

func TestPodmanLabelsWhoseSandboxItIs(t *testing.T) {
	p := &Podman{}
	spec := Spec{ID: "sbx_1", Name: "desk", Owner: "ada@example.com", Org: "org_1", Project: "prj_1"}
	args, _ := p.runArgs(spec, "")
	for _, label := range []string{
		podmanLabelOwner + "=ada@example.com", podmanLabelOrg + "=org_1", podmanLabelProject + "=prj_1",
	} {
		if !slices.Contains(args, label) {
			t.Errorf("no label %s: %v", label, args)
		}
	}
}

func TestSSHGreets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		say   string
		ready bool
	}{
		{"sshd", "SSH-2.0-OpenSSH_10.0\r\n", true},
		// rootless podman's forwarder accepts and closes while nothing listens.
		{"nothing behind the port", "", false},
		{"something else", "HTTP/1.1 400 Bad Request\r\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, client := net.Pipe()
			go func() {
				_, _ = io.WriteString(server, tc.say)
				_ = server.Close()
			}()
			defer client.Close()
			if got := sshGreets(client); got != tc.ready {
				t.Errorf("sshGreets = %v, want %v", got, tc.ready)
			}
		})
	}
}

// fakePodman is a podman that answers `inspect` with one container in the
// given state and fails everything else.
func fakePodman(t *testing.T, state string) *Podman {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "podman")
	script := "#!/bin/sh\nif [ \"$1\" = inspect ]; then echo '[{\"State\":" + state + "}]'; exit 0; fi\n" +
		"echo \"unexpected: $*\" >&2; exit 125\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Podman{opts: PodmanOptions{Binary: bin}, log: slog.New(slog.DiscardHandler)}
}

func TestPodmanDialRefusesASuspendedSandbox(t *testing.T) {
	p := fakePodman(t, `{"Status":"exited","Running":false,"ExitCode":0}`)
	_, err := p.Dial(context.Background(), Ref{ID: "sbx_1", Name: "desk"}, PortSSH)
	if !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "suspended") {
		t.Fatalf("Dial = %v, want ErrNotReady saying it is suspended", err)
	}
}

// A sandbox whose home volume is gone is not revived on a new, empty one.
func TestPodmanDoesNotReviveWithoutItsVolume(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "podman")
	script := "#!/bin/sh\nif [ \"$1\" = volume ]; then echo \"Error: no such volume $3\" >&2; exit 125; fi\n" +
		"echo \"unexpected: $*\" >&2; exit 125\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Podman{opts: PodmanOptions{Binary: bin}, log: slog.New(slog.DiscardHandler)}
	if err := p.Revive(context.Background(), Spec{ID: "sbx_1", Name: "desk"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Revive = %v, want ErrNotFound", err)
	}
}

func TestPodmanDialRefusesAPortThatIsNotPublished(t *testing.T) {
	p := fakePodman(t, `{"Status":"running","Running":true}`)
	_, err := p.Dial(context.Background(), Ref{ID: "sbx_1", Name: "desk"}, 8080)
	if _, ok := errors.AsType[*ErrRefused](err); !ok {
		t.Fatalf("Dial = %v, want a refusal naming the one port there is", err)
	}
}
