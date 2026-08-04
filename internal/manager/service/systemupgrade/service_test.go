package systemupgrade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckDetectsNewerRelease(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/latest" {
			t.Fatalf("path = %q, want /latest", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{
			"tag_name":"v0.8.10",
			"html_url":"https://github.com/ongridio/ongrid/releases/tag/v0.8.10",
			"published_at":"2026-06-10T00:00:00Z"
		}`)); err != nil {
			t.Fatalf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	svc := New(Config{
		CurrentVersion: "v0.8.4",
		ReleaseAPIURL:  srv.URL + "/latest",
		DownloadBase:   "https://ongrid.cloud/dl",
	}, srv.Client())
	info, err := svc.Check(context.Background())
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if !info.UpdateAvailable {
		t.Fatalf("UpdateAvailable = false, want true")
	}
	if !info.ComparisonSupported {
		t.Fatalf("ComparisonSupported = false, want true")
	}
	if info.LatestVersion != "v0.8.10" {
		t.Fatalf("LatestVersion = %q", info.LatestVersion)
	}
	if len(info.Commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(info.Commands))
	}
	if info.Commands[0].ID != "linux-universal" || info.Commands[0].Arch != "linux" {
		t.Fatalf("command = %+v, want one universal Linux command", info.Commands[0])
	}
	wantCommand := strings.Join([]string{
		"curl -fL -O https://ongrid.cloud/dl/ongrid-v0.8.10-linux.tar.xz || wget https://ongrid.cloud/dl/ongrid-v0.8.10-linux.tar.xz",
		"tar xf ongrid-v0.8.10-linux.tar.xz && cd ongrid-v0.8.10-linux",
		"sudo ./upgrade.sh",
	}, "\n")
	if info.Commands[0].Command != wantCommand {
		t.Fatalf("command = %q, want %q", info.Commands[0].Command, wantCommand)
	}
}

func TestCheckReportsNoUpdateWhenCurrentMatchesLatest(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"tag_name":"v0.8.4"}`)); err != nil {
			t.Fatalf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	svc := New(Config{CurrentVersion: "v0.8.4", ReleaseAPIURL: srv.URL}, srv.Client())
	info, err := svc.Check(context.Background())
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if info.UpdateAvailable {
		t.Fatalf("UpdateAvailable = true, want false")
	}
	if !info.ComparisonSupported {
		t.Fatalf("ComparisonSupported = false, want true")
	}
}

func TestCheckKeepsDevVersionNonComparable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"tag_name":"v0.8.4"}`)); err != nil {
			t.Fatalf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	svc := New(Config{CurrentVersion: "dev", ReleaseAPIURL: srv.URL}, srv.Client())
	info, err := svc.Check(context.Background())
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if info.ComparisonSupported {
		t.Fatalf("ComparisonSupported = true, want false")
	}
	if info.UpdateAvailable {
		t.Fatalf("UpdateAvailable = true, want false for non-comparable current")
	}
}

func TestCheckReturnsErrorOnBadReleaseResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	svc := New(Config{CurrentVersion: "v0.8.4", ReleaseAPIURL: srv.URL}, srv.Client())
	_, err := svc.Check(context.Background())
	if err == nil {
		t.Fatalf("Check returned nil error")
	}
	if !strings.Contains(err.Error(), "HTTP 429") {
		t.Fatalf("error = %v, want HTTP 429", err)
	}
}

func TestCheckUsesOfficialMetadataBeforeGitHubFallback(t *testing.T) {
	t.Parallel()
	var githubCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/latest.json":
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(`{
				"version":"v0.8.10",
				"release_url":"https://github.com/ongridio/ongrid/releases/tag/v0.8.10",
				"download_base":"https://mirror.example.test/dl"
			}`)); err != nil {
				t.Fatalf("write response: %v", err)
			}
		case "/github/latest":
			githubCalled = true
			http.Error(w, "github should not be called", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	svc := New(Config{
		CurrentVersion: "v0.8.4",
		ReleaseAPIURLs: []string{
			srv.URL + "/dl/latest.json",
			srv.URL + "/github/latest",
		},
	}, srv.Client())
	info, err := svc.Check(context.Background())
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if githubCalled {
		t.Fatalf("GitHub fallback was called despite official metadata success")
	}
	if !info.UpdateAvailable {
		t.Fatalf("UpdateAvailable = false, want true")
	}
	if !strings.Contains(info.Commands[0].Command, "https://mirror.example.test/dl") {
		t.Fatalf("command does not use metadata download_base: %s", info.Commands[0].Command)
	}
}

func TestCheckFallsBackToGitHubMetadata(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/latest.json":
			http.Error(w, "metadata not synced yet", http.StatusNotFound)
		case "/github/latest":
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(`{"tag_name":"v0.8.5"}`)); err != nil {
				t.Fatalf("write response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	svc := New(Config{
		CurrentVersion: "v0.8.4",
		ReleaseAPIURLs: []string{
			srv.URL + "/dl/latest.json",
			srv.URL + "/github/latest",
		},
	}, srv.Client())
	info, err := svc.Check(context.Background())
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if info.LatestVersion != "v0.8.5" {
		t.Fatalf("LatestVersion = %q, want v0.8.5", info.LatestVersion)
	}
	if !info.UpdateAvailable {
		t.Fatalf("UpdateAvailable = false, want true")
	}
}

func TestCheckAcceptsPlainTextVersionMetadata(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if _, err := w.Write([]byte("v0.8.6\n")); err != nil {
			t.Fatalf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	svc := New(Config{CurrentVersion: "v0.8.4", ReleaseAPIURL: srv.URL}, srv.Client())
	info, err := svc.Check(context.Background())
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if info.LatestVersion != "v0.8.6" {
		t.Fatalf("LatestVersion = %q, want v0.8.6", info.LatestVersion)
	}
}
