package setting

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	settingmodel "github.com/ongridio/ongrid/internal/manager/model/setting"
	"github.com/ongridio/ongrid/internal/pkg/errs"
	"github.com/ongridio/ongrid/internal/pkg/runner"
)

type recordingRunner struct {
	spec runner.Spec
}

func (r *recordingRunner) Isolation() runner.Isolation { return runner.IsolationNone }
func (r *recordingRunner) Run(_ context.Context, spec runner.Spec) (runner.Result, error) {
	r.spec = spec
	return runner.Result{}, nil
}

func TestObservabilityApplierUpdatesOnlyAllowedEnvAndRunsFixedCompose(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("KEEP=yes\nONGRID_PROM_RETENTION_TIME=2160h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commandRunner := &recordingRunner{}
	applier := newObservabilityApplier(New(newFakeRepo(), nil), observabilityApplyConfig{
		Workdir: dir, EnvFile: envFile, ComposeFiles: []string{"compose.yml"}, Project: "test",
	}, commandRunner, nil)
	err := applier.SaveAndApply(context.Background(), "prometheus", map[string]string{
		settingmodel.KeyPrometheusRetentionTime: "120h",
		settingmodel.KeyPrometheusRetentionSize: "2GB",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "KEEP=yes\nONGRID_PROM_RETENTION_SIZE=2GB\nONGRID_PROM_RETENTION_TIME=120h\n"
	if string(got) != want {
		t.Fatalf("env = %q, want %q", got, want)
	}
	wantArgv := []string{"docker", "compose", "--env-file", envFile, "-p", "test", "-f", "compose.yml", "up", "-d", "--no-deps", "--force-recreate", "prometheus"}
	if !reflect.DeepEqual(commandRunner.spec.Argv, wantArgv) {
		t.Fatalf("argv = %#v, want %#v", commandRunner.spec.Argv, wantArgv)
	}
}

type blockingRunner struct {
	started chan struct{}
	release chan struct{}
}

func (r *blockingRunner) Isolation() runner.Isolation { return runner.IsolationNone }
func (r *blockingRunner) Run(ctx context.Context, _ runner.Spec) (runner.Result, error) {
	close(r.started)
	select {
	case <-r.release:
		return runner.Result{}, nil
	case <-ctx.Done():
		return runner.Result{}, ctx.Err()
	}
}

func TestObservabilityApplierRejectsConcurrentSaveBeforePersistence(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("KEEP=yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := newFakeRepo()
	commandRunner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	applier := newObservabilityApplier(New(repo, nil), observabilityApplyConfig{
		Workdir: dir, EnvFile: envFile, ComposeFiles: []string{"compose.yml"},
	}, commandRunner, nil)

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- applier.SaveAndApply(context.Background(), "loki", map[string]string{
			settingmodel.KeyLokiRetentionPeriod: "720h",
		})
	}()
	<-commandRunner.started

	err := applier.SaveAndApply(context.Background(), "loki", map[string]string{
		settingmodel.KeyLokiRetentionPeriod: "48h",
	})
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("concurrent SaveAndApply() error = %v, want errs.ErrConflict", err)
	}
	if value, found, err := applier.settings.Get(context.Background(), settingmodel.CategoryObservability, settingmodel.KeyLokiRetentionPeriod); err != nil || !found || value != "720h" {
		t.Fatalf("persisted value = %q, found=%v, err=%v", value, found, err)
	}

	close(commandRunner.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}
