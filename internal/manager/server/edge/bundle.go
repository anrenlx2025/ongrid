// bundle.go — edge upgrade bundle resolver + static HTTP file
// server. The manager image bakes bundles at
// /usr/share/ongrid/edge-bundles/edge-bundle-<arch>-<version>.tar.gz
// (plus .sha256 companion); we expose them at
// /api/v1/edge-bundle/<arch>/<filename> so the edge can pull them
// over the same nginx pipeline it already trusts.
package edge

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ongridio/ongrid/internal/pkg/errs"
)

// FileBundleResolver implements PackageResolver against a local
// directory layout. Bundle files are named:
//
//	edge-bundle-<arch>-<version>.tar.gz
//	edge-bundle-<arch>-<version>.tar.gz.sha256
//
// (see dist/build-edge-bundle.sh). The resolver picks the bundle
// matching the requested (arch, version); empty version → "manager's
// own version" which the constructor takes verbatim. The returned URL
// is publicURL + the static route this same file registers; admins
// don't have to know the manager's listening port.
type FileBundleResolver struct {
	dir            string
	managerVersion string
	publicURL      string
}

// BundleCatalog describes the upgrade artifacts for the manager's current
// version. Every supported architecture is present even when its artifact is
// missing, which lets the UI block only the affected devices before dispatch.
type BundleCatalog struct {
	ManagerVersion string       `json:"manager_version"`
	Items          []BundleInfo `json:"items"`
}

type BundleInfo struct {
	Arch       string     `json:"arch"`
	Version    string     `json:"version"`
	Available  bool       `json:"available"`
	Bytes      int64      `json:"bytes,omitempty"`
	SHA256     string     `json:"sha256,omitempty"`
	ModifiedAt *time.Time `json:"modified_at,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// NewFileBundleResolver builds the resolver. dir typically
// /usr/share/ongrid/edge-bundles (set by Dockerfile.ongrid). publicURL
// is the manager's externally-reachable origin (no trailing slash);
// resolver constructs `{publicURL}/api/v1/edge-bundle/<arch>/<file>`.
func NewFileBundleResolver(dir, managerVersion, publicURL string) *FileBundleResolver {
	return &FileBundleResolver{
		dir:            strings.TrimRight(dir, "/"),
		managerVersion: managerVersion,
		publicURL:      strings.TrimRight(publicURL, "/"),
	}
}

// ResolveBundle implements PackageResolver.
func (r *FileBundleResolver) ResolveBundle(arch, version string) (url, sha256, resolvedVersion string, err error) {
	if r == nil {
		return "", "", "", errors.New("bundle resolver not wired")
	}
	if !knownArch(arch) {
		return "", "", "", fmt.Errorf("%w: unsupported arch %q", errs.ErrInvalid, arch)
	}
	if strings.TrimSpace(version) == "" {
		version = r.managerVersion
	}
	if version == "" {
		return "", "", "", errors.New("manager version unknown; cannot resolve bundle")
	}
	name := fmt.Sprintf("edge-bundle-%s-%s.tar.gz", arch, version)
	tarball := filepath.Join(r.dir, name)
	if _, err := os.Stat(tarball); err != nil {
		return "", "", "", fmt.Errorf("bundle missing: %s (this manager image may have been built without build-edge-bundle)", name)
	}
	sha, err := readBundleSHA(tarball + ".sha256")
	if err != nil {
		return "", "", "", fmt.Errorf("bundle sha file invalid: %s.sha256: %w", name, err)
	}
	if r.publicURL == "" {
		return "", "", "", errors.New("publicURL not configured; cannot build bundle URL")
	}
	// Bundle bytes are served by nginx from the same /edge/ static
	// path it already exposes for install.sh + individual binaries —
	// the bundle file lands next to them after install/upgrade.sh
	// extracts edge-bundles/ into bin/ (host) → /usr/share/nginx/
	// html/edge/ (container). Anonymous fetch; sha256 is the gate.
	return fmt.Sprintf("%s/edge/%s", r.publicURL, name), sha, version, nil
}

// CurrentBundles reports readiness of the manager-version artifacts. It does
// not expose filesystem paths or download URLs; callers only receive the
// metadata needed for a safe preflight.
func (r *FileBundleResolver) CurrentBundles() (BundleCatalog, error) {
	if r == nil {
		return BundleCatalog{}, errors.New("bundle resolver not wired")
	}
	version := strings.TrimSpace(r.managerVersion)
	if version == "" {
		return BundleCatalog{}, errors.New("manager version unknown; cannot inspect bundles")
	}
	catalog := BundleCatalog{ManagerVersion: version}
	for _, arch := range []string{"linux-amd64", "linux-arm64"} {
		catalog.Items = append(catalog.Items, r.inspectBundle(arch, version))
	}
	return catalog, nil
}

func (r *FileBundleResolver) inspectBundle(arch, version string) BundleInfo {
	item := BundleInfo{Arch: arch, Version: version}
	name := fmt.Sprintf("edge-bundle-%s-%s.tar.gz", arch, version)
	tarball := filepath.Join(r.dir, name)
	stat, err := os.Stat(tarball)
	if err != nil {
		item.Error = "bundle file is missing"
		return item
	}
	if !stat.Mode().IsRegular() {
		item.Error = "bundle path is not a regular file"
		return item
	}
	sha, err := readBundleSHA(tarball + ".sha256")
	if err != nil {
		item.Error = "checksum file is missing or invalid"
		return item
	}
	if strings.TrimSpace(r.publicURL) == "" {
		item.Error = "public download URL is not configured"
		return item
	}
	modifiedAt := stat.ModTime().UTC()
	item.Available = true
	item.Bytes = stat.Size()
	item.SHA256 = sha
	item.ModifiedAt = &modifiedAt
	return item
}

func readBundleSHA(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(raw))
	if len(value) < 64 {
		return "", errors.New("checksum is shorter than 64 characters")
	}
	value = value[:64]
	if _, err := hex.DecodeString(value); err != nil {
		return "", errors.New("checksum is not hexadecimal")
	}
	return strings.ToLower(value), nil
}

func knownArch(a string) bool {
	switch a {
	case "linux-amd64":
		return true
	case "linux-arm64":
		return true
	default:
		return false
	}
}
