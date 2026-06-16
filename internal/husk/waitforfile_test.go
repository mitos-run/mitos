package husk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitForFileAppears(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "rootfs.ext4")
	go func() { time.Sleep(300 * time.Millisecond); os.WriteFile(p, []byte("x"), 0o644) }()
	if err := waitForFile(context.Background(), p, 5*time.Second); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}
func TestWaitForFileTimeout(t *testing.T) {
	if err := waitForFile(context.Background(), "/nonexistent/rootfs.ext4", 200*time.Millisecond); err == nil {
		t.Fatal("want timeout error")
	}
}
