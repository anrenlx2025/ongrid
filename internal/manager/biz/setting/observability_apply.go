package setting

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	settingmodel "github.com/ongridio/ongrid/internal/manager/model/setting"
	"github.com/ongridio/ongrid/internal/pkg/errs"
	"github.com/ongridio/ongrid/internal/pkg/runner"
)

// observabilityApplyConfig enables the deliberately opt-in Compose applicator.
// It is intended for test installations where the manager runs on the Compose
// host; normal deployments keep it disabled and never grant Docker access.
type observabilityApplyConfig struct {
	Workdir      string
	EnvFile      string
	ComposeFiles []string
	Project      string
}

type ObservabilityApplier struct {
	settings *Service
	cfg      observabilityApplyConfig
	runner   runner.Runner
	log      *slog.Logger
	applying atomic.Bool
}

func NewObservabilityApplierFromEnv(settings *Service, log *slog.Logger) *ObservabilityApplier {
	var files []string
	for _, file := range strings.Split(os.Getenv("ONGRID_OBSERVABILITY_COMPOSE_FILES"), ",") {
		if file = strings.TrimSpace(file); file != "" {
			files = append(files, file)
		}
	}
	return newObservabilityApplier(settings, observabilityApplyConfig{
		Workdir:      strings.TrimSpace(os.Getenv("ONGRID_OBSERVABILITY_COMPOSE_DIR")),
		EnvFile:      strings.TrimSpace(os.Getenv("ONGRID_OBSERVABILITY_ENV_FILE")),
		ComposeFiles: files,
		Project:      strings.TrimSpace(os.Getenv("ONGRID_OBSERVABILITY_COMPOSE_PROJECT")),
	}, runner.NewShellRunner(), log)
}

func newObservabilityApplier(settings *Service, cfg observabilityApplyConfig, commandRunner runner.Runner, log *slog.Logger) *ObservabilityApplier {
	if log == nil {
		log = slog.Default()
	}
	return &ObservabilityApplier{settings: settings, cfg: cfg, runner: commandRunner, log: log.With(slog.String("component", "observability-apply"))}
}

func (a *ObservabilityApplier) Enabled() bool {
	return a != nil && a.settings != nil && a.runner != nil && a.cfg.Workdir != "" && a.cfg.EnvFile != "" && len(a.cfg.ComposeFiles) > 0
}

func (a *ObservabilityApplier) SaveAndApply(ctx context.Context, service string, values map[string]string) (err error) {
	if !a.Enabled() {
		return fmt.Errorf("%w: direct observability apply is disabled", errs.ErrNotWiredYet)
	}
	envValues, err := observabilityEnvValues(service, values)
	if err != nil {
		return err
	}
	// ponytail: process-local gating fits single-Manager test stacks; use a
	// file lock only if multi-process host-side apply is ever supported.
	if !a.applying.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: another observability apply is in progress", errs.ErrConflict)
	}
	defer func() {
		a.applying.Store(false)
		if err != nil {
			a.log.Error("apply built-in observability storage limits", slog.String("service", service), slog.Any("err", err))
		}
	}()

	settings := make([]settingmodel.Setting, 0, len(values))
	for key, value := range values {
		settings = append(settings, settingmodel.Setting{
			Category: settingmodel.CategoryObservability,
			Key:      key,
			Value:    value,
		})
	}
	if err := a.settings.SetBatch(ctx, settings); err != nil {
		return err
	}

	info, err := os.Lstat(a.cfg.EnvFile)
	if err != nil {
		return fmt.Errorf("read observability env file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: observability env file must be a regular file", errs.ErrInvalid)
	}
	before, err := os.ReadFile(a.cfg.EnvFile)
	if err != nil {
		return fmt.Errorf("read observability env file: %w", err)
	}
	if err := writeFileAtomic(a.cfg.EnvFile, updateEnv(before, envValues), info.Mode().Perm()); err != nil {
		return fmt.Errorf("update observability env file: %w", err)
	}

	applyErr := a.runCompose(ctx, service)
	if applyErr == nil {
		a.log.Info("built-in observability storage limits applied", slog.String("service", service))
		return nil
	}

	restoreErr := writeFileAtomic(a.cfg.EnvFile, before, info.Mode().Perm())
	if restoreErr != nil {
		return errors.Join(applyErr, fmt.Errorf("restore observability env file: %w", restoreErr))
	}
	if rollbackErr := a.runCompose(context.WithoutCancel(ctx), service); rollbackErr != nil {
		return errors.Join(applyErr, fmt.Errorf("restore built-in %s: %w", service, rollbackErr))
	}
	return applyErr
}

func (a *ObservabilityApplier) runCompose(ctx context.Context, service string) error {
	args := []string{"docker", "compose", "--env-file", a.cfg.EnvFile}
	if a.cfg.Project != "" {
		args = append(args, "-p", a.cfg.Project)
	}
	for _, file := range a.cfg.ComposeFiles {
		args = append(args, "-f", file)
	}
	args = append(args, "up", "-d", "--no-deps", "--force-recreate", service)
	env := map[string]string{"PATH": os.Getenv("PATH")}
	if home, err := os.UserHomeDir(); err == nil {
		env["HOME"] = home
	}
	for _, key := range []string{"DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_HOST"} {
		if value := os.Getenv(key); value != "" {
			env[key] = value
		}
	}
	result, err := a.runner.Run(ctx, runner.Spec{
		Argv:           args,
		Env:            env,
		Workdir:        a.cfg.Workdir,
		Timeout:        2 * time.Minute,
		MaxOutputBytes: 32 << 10,
	})
	if err != nil {
		return fmt.Errorf("apply built-in %s limits: %w", service, err)
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = strings.TrimSpace(result.Stdout)
		}
		return fmt.Errorf("apply built-in %s limits: docker compose exited %d: %s", service, result.ExitCode, message)
	}
	return nil
}

func observabilityEnvValues(service string, values map[string]string) (map[string]string, error) {
	keys := map[string]map[string]string{
		"prometheus": {
			settingmodel.KeyPrometheusRetentionTime: "ONGRID_PROM_RETENTION_TIME",
			settingmodel.KeyPrometheusRetentionSize: "ONGRID_PROM_RETENTION_SIZE",
		},
		"loki": {
			settingmodel.KeyLokiRetentionPeriod: "ONGRID_LOKI_RETENTION_PERIOD",
		},
		"tempo": {
			settingmodel.KeyTempoBlockRetention: "ONGRID_TEMPO_BLOCK_RETENTION",
		},
	}
	allowed, ok := keys[service]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported observability service %q", errs.ErrInvalid, service)
	}
	if len(values) != len(allowed) {
		return nil, fmt.Errorf("%w: incomplete observability settings for %s", errs.ErrInvalid, service)
	}
	out := make(map[string]string, len(allowed))
	for key, envKey := range allowed {
		value, ok := values[key]
		if !ok {
			return nil, fmt.Errorf("%w: missing observability setting %q", errs.ErrInvalid, key)
		}
		out[envKey] = value
	}
	return out, nil
}

func updateEnv(before []byte, values map[string]string) []byte {
	lines := strings.Split(strings.TrimSuffix(string(before), "\n"), "\n")
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	filtered := lines[:0]
	for _, line := range lines {
		key, _, found := strings.Cut(line, "=")
		if found {
			if _, replaced := values[key]; replaced {
				continue
			}
		}
		filtered = append(filtered, line)
	}
	for len(filtered) > 0 && filtered[len(filtered)-1] == "" {
		filtered = filtered[:len(filtered)-1]
	}
	for _, key := range keys {
		filtered = append(filtered, key+"="+values[key])
	}
	return []byte(strings.Join(filtered, "\n") + "\n")
}

func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".observability-env-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
