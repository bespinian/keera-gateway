package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryLimit(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		p := filepath.Join(dir, "memory.max")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if got, ok := memoryLimit(write("1073741820\n")); !ok || got != 966367638 {
		t.Errorf("limit = %d, %v; want nine tenths of the container's", got, ok)
	}
	if _, ok := memoryLimit(write("max\n")); ok {
		t.Error("a container without a limit got one")
	}
	if _, ok := memoryLimit(filepath.Join(dir, "missing")); ok {
		t.Error("a host without cgroup v2 got a limit")
	}
}
