package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ongridio/ongrid/internal/pkg/tunnel"
)

const (
	logsExternalBackend = "external_elasticsearch"
	logsESSecretSlot    = "elasticsearch_api_key"
)

var logsProbeIDPattern = regexp.MustCompile(`^ongrid-log-probe-[A-Za-z0-9_-]{20,64}$`)

func (t *TunnelConfigFetcher) materializeLogsRuntime(ctx context.Context, cfg PluginConfig) (PluginConfig, error) {
	spec := copySpec(cfg.Spec)
	backend := configString(spec, "backend")
	shadow, _ := spec["rollout_shadow"].(bool)
	probeID := configString(spec, "log_probe_id")
	if backend != logsExternalBackend && !shadow && probeID == "" {
		cfg.Spec = spec
		return cfg, nil
	}
	generation, err := uint64Spec(spec["backend_generation"])
	if err != nil || generation == 0 {
		return PluginConfig{}, errors.New("logs backend generation is required")
	}
	dir := filepath.Join(t.secretBaseDir, "logs")
	if backend == logsExternalBackend {
		slot := configString(spec, "elasticsearch_secret_slot")
		keyPath, keyErr := t.fetchAndMaterializeESKey(ctx, dir, generation, slot)
		if keyErr != nil {
			return PluginConfig{}, keyErr
		}
		spec["elasticsearch_api_key_file"] = keyPath
		delete(spec, "elasticsearch_secret_slot")

		if caPEM := configString(spec, "elasticsearch_ca_pem"); caPEM != "" {
			caPath := filepath.Join(dir, fmt.Sprintf("elasticsearch_ca.g%d.pem", generation))
			if err := atomicWriteRestricted(dir, caPath, []byte(caPEM+"\n"), 0o600); err != nil {
				return PluginConfig{}, fmt.Errorf("write Elasticsearch CA: %w", err)
			}
			spec["elasticsearch_ca_file"] = caPath
		}
		delete(spec, "elasticsearch_ca_pem")
	}

	if probeID != "" {
		if !logsProbeIDPattern.MatchString(probeID) {
			return PluginConfig{}, errors.New("invalid logs probe id")
		}
		probePath := filepath.Join(dir, logsProbeFilename(generation, probeID))
		if err := atomicWriteRestricted(dir, probePath, []byte(probeID+"\n"), 0o600); err != nil {
			return PluginConfig{}, fmt.Errorf("write logs probe: %w", err)
		}
		spec["log_probe_file"] = probePath
	}

	if shadow && configString(spec, "baseline_backend") == logsExternalBackend {
		baselineGeneration, generationErr := uint64Spec(spec["baseline_backend_generation"])
		if generationErr != nil || baselineGeneration == 0 || (backend == logsExternalBackend && baselineGeneration == generation) {
			return PluginConfig{}, errors.New("external Elasticsearch baseline generation is invalid")
		}
		baselineSlot := configString(spec, "baseline_elasticsearch_secret_slot")
		baselineKeyPath, keyErr := t.fetchAndMaterializeESKey(ctx, dir, baselineGeneration, baselineSlot)
		if keyErr != nil {
			return PluginConfig{}, fmt.Errorf("materialize baseline Elasticsearch credential: %w", keyErr)
		}
		spec["baseline_elasticsearch_api_key_file"] = baselineKeyPath
		delete(spec, "baseline_elasticsearch_secret_slot")
		if caPEM := configString(spec, "baseline_elasticsearch_ca_pem"); caPEM != "" {
			caPath := filepath.Join(dir, fmt.Sprintf("elasticsearch_ca.g%d.pem", baselineGeneration))
			if err := atomicWriteRestricted(dir, caPath, []byte(caPEM+"\n"), 0o600); err != nil {
				return PluginConfig{}, fmt.Errorf("write baseline Elasticsearch CA: %w", err)
			}
			spec["baseline_elasticsearch_ca_file"] = caPath
		}
		delete(spec, "baseline_elasticsearch_ca_pem")
	}
	cfg.Spec = spec
	return cfg, nil
}

// logsProbeFilename gives every Manager-issued probe a distinct path, even
// when a retry reuses the same backend generation. The filelog receiver keeps
// persistent offsets by file identity; overwriting a same-length token at the
// old path would otherwise leave its offset at EOF and the retry invisible.
func logsProbeFilename(generation uint64, probeID string) string {
	digest := sha256.Sum256([]byte(probeID))
	return fmt.Sprintf("logs_probe.g%d.%s.log", generation, hex.EncodeToString(digest[:8]))
}

func (t *TunnelConfigFetcher) fetchAndMaterializeESKey(ctx context.Context, dir string, generation uint64, slot string) (string, error) {
	if slot != logsESSecretSlot {
		return "", errors.New("unsupported Elasticsearch secret slot")
	}
	var secret tunnel.GetPluginSecretResponse
	if err := t.client.Call(ctx, tunnel.MethodGetPluginSecret, tunnel.GetPluginSecretRequest{
		Plugin: "logs", Slot: slot, Generation: generation,
	}, &secret); err != nil {
		return "", fmt.Errorf("fetch Elasticsearch API key generation %d: %w", generation, err)
	}
	if secret.Generation != generation || strings.TrimSpace(secret.Content) == "" {
		return "", errors.New("invalid Elasticsearch secret response")
	}
	digest := sha256.Sum256([]byte(secret.Content))
	if !strings.EqualFold(secret.SHA256, hex.EncodeToString(digest[:])) {
		return "", errors.New("Elasticsearch secret checksum mismatch")
	}
	keyPath := filepath.Join(dir, fmt.Sprintf("elasticsearch_api_key.g%d", generation))
	if err := materializeGenerationFile(dir, keyPath, generation, []byte(secret.Content)); err != nil {
		return "", fmt.Errorf("write Elasticsearch API key: %w", err)
	}
	return keyPath, nil
}

// ReportPluginConfigApplied implements ConfigApplyReporter. Only a rollout
// config carrying a Manager-issued probe id is acknowledged; ordinary built-in
// or already-active snapshots do not create control-plane noise.
func (t *TunnelConfigFetcher) ReportPluginConfigApplied(ctx context.Context, plugin string, cfg PluginConfig, applyErr error) error {
	if t.client == nil || plugin != "logs" {
		return nil
	}
	probeID := configString(cfg.Spec, "log_probe_id")
	if probeID == "" {
		return nil
	}
	if !logsProbeIDPattern.MatchString(probeID) {
		return errors.New("refusing invalid logs probe acknowledgement")
	}
	generation, err := uint64Spec(cfg.Spec["backend_generation"])
	if err != nil || generation == 0 {
		return errors.New("refusing invalid logs generation acknowledgement")
	}
	request := tunnel.ReportPluginConfigAppliedRequest{
		Plugin: "logs", Generation: generation, Applied: applyErr == nil, ProbeID: probeID,
	}
	if applyErr != nil {
		request.ErrorClass = configApplyErrorClass(applyErr)
	}
	var response tunnel.ReportPluginConfigAppliedResponse
	if err := t.client.Call(ctx, tunnel.MethodReportPluginConfigApplied, request, &response); err != nil {
		return fmt.Errorf("report logs generation: %w", err)
	}
	if !response.OK {
		return errors.New("manager rejected logs generation acknowledgement")
	}
	return nil
}

func configApplyErrorClass(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, os.ErrPermission), strings.Contains(message, "read-only file system"),
		strings.Contains(message, "operation not permitted"):
		return "secret_materialization_failed"
	case strings.Contains(message, "validate"), strings.Contains(message, "configuration"):
		return "collector_config_rejected"
	case strings.Contains(message, "binary missing"), strings.Contains(message, "no such file"):
		return "collector_binary_missing"
	case strings.Contains(message, "readiness"), strings.Contains(message, "deadline"):
		return "collector_not_ready"
	default:
		return "collector_start_failed"
	}
}

func configString(spec map[string]interface{}, key string) string {
	if spec == nil {
		return ""
	}
	value, ok := spec[key]
	if !ok || value == nil {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func materializeGenerationFile(dir, path string, generation uint64, content []byte) error {
	if generation == 0 {
		return errors.New("generation must be positive")
	}
	if len(content) == 0 || len(content) > 1<<20 {
		return errors.New("secret content size is invalid")
	}
	generationPath := path + ".generation"
	if raw, err := os.ReadFile(generationPath); err == nil {
		current, parseErr := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
		if parseErr != nil {
			return errors.New("stored secret generation is invalid")
		}
		if generation < current {
			return fmt.Errorf("secret generation %d is older than local generation %d", generation, current)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := atomicWriteRestricted(dir, path, content, 0o600); err != nil {
		return err
	}
	return atomicWriteRestricted(dir, generationPath, []byte(strconv.FormatUint(generation, 10)+"\n"), 0o600)
}

func atomicWriteRestricted(dir, path string, content []byte, mode os.FileMode) error {
	if err := ensurePrivateDirectory(dir); err != nil {
		return err
	}
	if filepath.Clean(filepath.Dir(path)) != filepath.Clean(dir) {
		return errors.New("secret path escapes managed directory")
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to replace secret symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ongrid-secret-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	defer cleanup()
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
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func ensurePrivateDirectory(dir string) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(parent); err != nil {
		return err
	} else if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing secret directory below symlink")
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("managed secret directory is unsafe")
	}
	return os.Chmod(dir, 0o700)
}

func uint64Spec(value interface{}) (uint64, error) {
	switch typed := value.(type) {
	case uint64:
		return typed, nil
	case uint:
		return uint64(typed), nil
	case int:
		if typed > 0 {
			return uint64(typed), nil
		}
	case int64:
		if typed > 0 {
			return uint64(typed), nil
		}
	case float64:
		if typed > 0 && typed == float64(uint64(typed)) {
			return uint64(typed), nil
		}
	case string:
		return strconv.ParseUint(strings.TrimSpace(typed), 10, 64)
	}
	return 0, errors.New("invalid generation")
}
