//go:build linux

package packetcapture

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestServiceStartIsIdempotentAndSerializesCaptures(t *testing.T) {
	svc := newServiceForTest(t, func(ctx context.Context, _ Request) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	})
	first, err := svc.Start(Request{CaptureID: "capture-123", Interface: "eth0"})
	if err != nil {
		t.Fatalf("Start first: %v", err)
	}
	second, err := svc.Start(Request{CaptureID: "capture-123", Interface: "eth0"})
	if err != nil {
		t.Fatalf("Start duplicate: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("duplicate IDs: %q %q", first.ID, second.ID)
	}
	if _, err := svc.Start(Request{CaptureID: "capture-456", Interface: "eth0"}); err == nil {
		t.Fatal("concurrent capture was accepted")
	}
	if _, err := svc.Cancel(first.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}

func TestServiceCompletesTask(t *testing.T) {
	var once sync.Once
	svc := newServiceForTest(t, func(context.Context, Request) (Result, error) {
		once.Do(func() {})
		return Result{StopReason: "duration_limit"}, nil
	})
	if _, err := svc.Start(Request{CaptureID: "capture-123", Interface: "eth0"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		task, ok := svc.Get("capture-123")
		if ok && task.State == TaskSucceeded {
			return
		}
		time.Sleep(time.Millisecond)
	}
	task, _ := svc.Get("capture-123")
	t.Fatalf("task did not complete: %+v", task)
}

func TestServiceReadCompletedCapture(t *testing.T) {
	dir := t.TempDir()
	svc := newServiceForDirTest(t, dir, func(_ context.Context, in Request) (Result, error) {
		path := filepath.Join(dir, in.CaptureID+".pcap")
		if err := os.WriteFile(path, []byte("pcap-data"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return Result{Path: path, FileBytes: 9, StopReason: "duration_limit"}, nil
	})
	if _, err := svc.Start(Request{CaptureID: "capture-123", Interface: "eth0"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		task, _ := svc.Get("capture-123")
		if task.State == TaskSucceeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not complete: %+v", task)
		}
		time.Sleep(time.Millisecond)
	}
	raw, err := svc.Read("capture-123", 1024)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(raw.Data) != "pcap-data" || raw.SizeBytes != 9 || raw.SHA256Hex == "" {
		t.Fatalf("raw = %+v", raw)
	}
	if _, err := svc.Read("capture-123", 4); err == nil {
		t.Fatal("Read accepted object over limit")
	}
}

func newServiceForTest(t *testing.T, runner func(context.Context, Request) (Result, error)) *Service {
	t.Helper()
	return newServiceForDirTest(t, t.TempDir(), runner)
}

func newServiceForDirTest(t *testing.T, dir string, runner func(context.Context, Request) (Result, error)) *Service {
	t.Helper()
	capturer, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc, err := NewService(capturer)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.runner = runner
	return svc
}
