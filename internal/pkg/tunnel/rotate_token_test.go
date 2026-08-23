package tunnel

import (
	"encoding/json"
	"sync/atomic"
	"testing"

	gclient "github.com/singchia/geminio/client"
)

// TestUpdateCredentials_NoPanicsBeforeDial verifies that calling
// UpdateCredentials before Dial (endOpts still nil) returns safely
// instead of panicking.
func TestUpdateCredentials_NoPanicsBeforeDial(t *testing.T) {
	c := NewClient(ClientConfig{
		ServerAddr: "127.0.0.1:0",
		AccessKey:  "old-ak",
		SecretKey:  "old-cred",
	})
	gc := c.(*geminioClient)

	// endOpts is not initialized yet; this must not panic.
	gc.UpdateCredentials("new-ak", "new-cred")

	if gc.cfg.AccessKey != "new-ak" {
		t.Fatalf("AccessKey = %q, want %q", gc.cfg.AccessKey, "new-ak")
	}
	if gc.cfg.SecretKey != "new-cred" {
		t.Fatalf("SecretKey = %q, want %q", gc.cfg.SecretKey, "new-cred")
	}
}

// TestUpdateCredentials_UpdatesEndOptsMeta verifies that after
// UpdateCredentials, endOpts.Meta holds the JSON serialization of the
// new credentials.
func TestUpdateCredentials_UpdatesEndOptsMeta(t *testing.T) {
	c := NewClient(ClientConfig{
		ServerAddr: "127.0.0.1:0",
		AccessKey:  "ak",
		SecretKey:  "original",
	})
	gc := c.(*geminioClient)

	// Simulate a Dial that has populated endOpts.
	oldMeta, _ := json.Marshal(Meta{
		AccessKey: "ak",
		SecretKey: "original",
	})
	gc.endOpts = newTestEndOptions(oldMeta)

	gc.UpdateCredentials("ak", "rotated")

	var m Meta
	if err := json.Unmarshal(gc.endOpts.Meta, &m); err != nil {
		t.Fatalf("unmarshal endOpts.Meta: %v", err)
	}
	if m.SecretKey != "rotated" {
		t.Fatalf("endOpts.Meta SecretKey = %q, want %q", m.SecretKey, "rotated")
	}
	if m.AccessKey != "ak" {
		t.Fatalf("endOpts.Meta AccessKey = %q, want %q (should not change)", m.AccessKey, "ak")
	}
}

// TestRotateTokenMessageTypes checks the basic serialization behavior of
// the rotate_token constants and types.
func TestRotateTokenMessageTypes(t *testing.T) {
	if MethodRotateToken != "rotate_token" {
		t.Fatalf("MethodRotateToken = %q, want %q", MethodRotateToken, "rotate_token")
	}

	// RotateTokenResponse round-trip.
	resp := RotateTokenResponse{SecretKey: "rotated-cred-value"}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var decoded RotateTokenResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if decoded.SecretKey != "rotated-cred-value" {
		t.Fatalf("SecretKey round-trip = %q, want %q", decoded.SecretKey, "rotated-cred-value")
	}
}

// TestUpdateCredentials_ConcurrentSafe verifies UpdateCredentials does
// not panic under concurrent calls.
func TestUpdateCredentials_ConcurrentSafe(t *testing.T) {
	c := NewClient(ClientConfig{
		ServerAddr: "127.0.0.1:0",
		AccessKey:  "ak",
		SecretKey:  "cred",
	})
	gc := c.(*geminioClient)
	gc.endOpts = newTestEndOptions(nil)

	var done atomic.Int32
	for i := 0; i < 10; i++ {
		go func() {
			gc.UpdateCredentials("ak", "cred-updated")
			done.Add(1)
		}()
	}
	for done.Load() < 10 {
		// Wait for all goroutines to finish.
	}
}

// newTestEndOptions builds EndOptions for tests without a real network
// dial.
func newTestEndOptions(meta []byte) *gclient.EndOptions {
	opt := gclient.NewEndOptions()
	if meta != nil {
		opt.SetMeta(meta)
	}
	return opt
}
