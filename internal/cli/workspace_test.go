package cli

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAskExperience(t *testing.T) {
	for in, want := range map[string]string{
		"1\n": "terminal", "terminal\n": "terminal", "2\n": "vscode", " VS Code \n": "vscode", "2": "vscode",
	} {
		got, err := askExperience(strings.NewReader(in), io.Discard)
		if err != nil || got != want {
			t.Errorf("askExperience(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := askExperience(strings.NewReader("3\n"), io.Discard); err == nil {
		t.Error("an answer that is neither was taken")
	}
}

func TestWithSSHBlock(t *testing.T) {
	block := sshBlock("/usr/local/bin/keera", "https://keera.example.ch", "/home/ada/.ssh/keera_ed25519")
	if !strings.Contains(block, "ProxyCommand /usr/local/bin/keera --url https://keera.example.ch workspace ssh-proxy %h") {
		t.Errorf("the block does not run the proxy:\n%s", block)
	}
	if got := withSSHBlock("", block); got != block {
		t.Errorf("into an empty config:\n%s", got)
	}
	mine := "Host *\n  ServerAliveInterval 30\n"
	once := withSSHBlock(mine, block)
	if !strings.HasPrefix(once, block) || !strings.HasSuffix(once, mine) {
		t.Errorf("the block is not first, or the rest is lost:\n%s", once)
	}
	// Again, with another address: replaced, not added.
	other := sshBlock("/usr/local/bin/keera", "https://other.example.ch", "/k")
	twice := withSSHBlock(once, other)
	if strings.Count(twice, sshBlockStart) != 1 || !strings.Contains(twice, "other.example.ch") ||
		!strings.HasSuffix(twice, mine) {
		t.Errorf("a second run:\n%s", twice)
	}
	// Options above the first Host line apply to every host. Put after the
	// block, they must still, so the block's section has to end.
	global := "Include ~/.orbstack/ssh/config\nIdentityFile ~/.ssh/id_work\nHost github.com\n  User git\n"
	got := withSSHBlock(global, block)
	keeraAt, matchAt := strings.Index(got, "Host *"+sshHostSuffix), strings.Index(got, "\nMatch all\n")
	if matchAt < keeraAt || matchAt > strings.Index(got, "Include ~/.orbstack") {
		t.Errorf("the workspace section does not end before the config's own options:\n%s", got)
	}
	if quoteSSH("/Users/Ada Lovelace/keera") != `"/Users/Ada Lovelace/keera"` {
		t.Error("a path with a space is not quoted")
	}
}

func TestStreamThroughKeeraAgents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/dev/connect" || r.URL.Query().Get("port") != "ssh" ||
			r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Upgrade") != "keera-stream" {
			http.Error(w, `{"error":{"message":"no such workspace"}}`, http.StatusNotFound)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: keera-stream\r\n\r\n")
		_ = rw.Flush()
		line, _ := rw.ReadString('\n')
		_, _ = conn.Write([]byte("workspace: " + line))
	}))
	defer srv.Close()
	a := &agents{base: srv.URL, token: "tok", http: srv.Client()}

	conn, err := a.stream(t.Context(), "dev", "ssh")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte("SSH-2.0-test\n"))
	if got, _ := bufio.NewReader(conn).ReadString('\n'); got != "workspace: SSH-2.0-test\n" {
		t.Errorf("read %q", got)
	}

	if _, err := a.stream(t.Context(), "nope", "ssh"); err == nil ||
		!strings.Contains(err.Error(), "no such workspace") {
		t.Errorf("a refused stream: %v", err)
	}
}
