// extract_pending.go — Windows 兼容层：解压 worker agent_upgrade RPC 下载的
// pending tar.gz 到 incoming/。
// 背景：Linux edge 由 systemd ExecStartPre（apply-pending-upgrade.sh）完成
// pending → incoming 解压；Windows 无对等机制，由 supervisor CheckPending
// 调用本文件函数完成。
package upgrademachine

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// PendingFileName 是 worker agent_upgrade RPC 下载的 bundle 文件名（tar.gz 内容，
// 无 .tar.gz 后缀）。对称 biz/upgrade.go 的 final path。
const PendingFileName = "pending"

// PendingSHA256FileName 是 pending 的 sha256 companion 文件名。
const PendingSHA256FileName = "pending.sha256"

// maxExtractBytes 限制解压后总大小（1 GB，对称 upgradebundle.maxBundleBytes）。
const maxExtractBytes = 1024 * 1024 * 1024

// HasPendingBundle 报告 {stageDir}/pending 文件是否存在。
// 用于区分"无 upgrade"和"pending tar.gz 未解压"两种状态。
func HasPendingBundle(stageDir string) bool {
	info, err := os.Stat(filepath.Join(stageDir, PendingFileName))
	return err == nil && !info.IsDir()
}

// ExtractPendingBundle 解压 {stageDir}/pending（tar.gz）到 {stageDir}/incoming/，
// 然后删除 pending + pending.sha256（防止重复解压触发 upgrade 死循环）。
// 调用方：Machine.CheckPending 在 IsPending 返回 false 但 HasPendingBundle
// 返回 true 时调用（Windows supervisor 路径）。
//
// 消费前先复核 pending 的 sha256 与 worker 下载时写的 companion 一致
// （纵深：worker 侧下载时验过一次，supervisor 消费前再验一次 — 攻击者若
// 只拿到 pending 的写权限而无 pending.sha256 的写权限即被拦）。复核失败
// 或 companion 缺失 = fail-closed：删除 pending + sha（防死循环）并返回 error。
func ExtractPendingBundle(stageDir string) error {
	pendingPath := filepath.Join(stageDir, PendingFileName)
	shaPath := filepath.Join(stageDir, PendingSHA256FileName)
	incomingPath := IncomingDir(stageDir)

	if err := verifyPendingSHA256(pendingPath, shaPath); err != nil {
		// 完整性不可信的 pending 不得进入解压；删除防 CheckPending 死循环。
		_ = os.Remove(pendingPath)
		_ = os.Remove(shaPath)
		return fmt.Errorf("verify pending: %w", err)
	}

	// 清理旧 incoming/ 残留（幂等）
	_ = os.RemoveAll(incomingPath)

	if err := extractTarGz(pendingPath, incomingPath); err != nil {
		return fmt.Errorf("extract pending: %w", err)
	}

	// 解压成功后删除 pending + pending.sha256（防止 CheckPending 重复触发）
	if err := os.Remove(pendingPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove pending: %w", err)
	}
	_ = os.Remove(shaPath)

	return nil
}

// verifyPendingSHA256 计算 pending 文件的 sha256 并与 companion 文件内容比对。
// companion 由 worker 侧下载链路写入（格式：64 位小写 hex + 换行，对称
// biz/upgrade.go）。companion 缺失 = fail-closed（本 PR 起 worker 下载链路
// 恒写该文件，缺失说明状态不完整）。
func verifyPendingSHA256(pendingPath, shaPath string) error {
	want, err := os.ReadFile(shaPath)
	if err != nil {
		return fmt.Errorf("read sha companion (missing = fail-closed): %w", err)
	}
	expected := strings.ToLower(strings.TrimSpace(string(want)))
	if len(expected) != 64 {
		return fmt.Errorf("sha companion malformed (want 64 hex chars, got %d)", len(expected))
	}
	f, err := os.Open(pendingPath)
	if err != nil {
		return fmt.Errorf("open pending: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash pending: %w", err)
	}
	if got := fmt.Sprintf("%x", h.Sum(nil)); got != expected {
		return fmt.Errorf("checksum mismatch (want %s, got %s)", expected, got)
	}
	return nil
}

// extractTarGz 解压 src（.tar.gz）到 dst。
// 安全措施：拒绝路径逃逸（zip-slip）、symlink/hardlink、超限总大小。
// 逻辑对称 upgradebundle/download.go 的 extractTarGz（副本，避免循环依赖）。
func extractTarGz(src, dst string) error {
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return fmt.Errorf("mkdir dst: %w", err)
	}
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var totalBytes int64
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return fmt.Errorf("abs dst: %w", err)
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		// 只接受普通文件和目录（拒绝 symlink/hardlink/device）。
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir:
		default:
			return fmt.Errorf("disallowed tar entry type %v in %q", hdr.Typeflag, hdr.Name)
		}
		cleaned := filepath.Clean(hdr.Name)
		if strings.HasPrefix(cleaned, "/") || strings.Contains(cleaned, "..") {
			return fmt.Errorf("disallowed tar path %q (escapes archive root)", hdr.Name)
		}
		target := filepath.Join(absDst, cleaned)
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return fmt.Errorf("abs target: %w", err)
		}
		if !EqualFoldPrefix(absTarget, absDst+string(os.PathSeparator)) && !strings.EqualFold(absTarget, absDst) {
			return fmt.Errorf("target %q escapes %q", absTarget, absDst)
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mkdir parent of %s: %w", target, err)
		}
		w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
			os.FileMode(hdr.Mode)&0o755)
		if err != nil {
			return fmt.Errorf("open out %s: %w", target, err)
		}
		n, err := io.Copy(w, io.LimitReader(tr, maxExtractBytes-totalBytes+1))
		if err != nil {
			_ = w.Close()
			return fmt.Errorf("write %s: %w", target, err)
		}
		if err := w.Close(); err != nil {
			return fmt.Errorf("close %s: %w", target, err)
		}
		totalBytes += n
		if totalBytes > maxExtractBytes {
			return fmt.Errorf("extracted size exceeded %d bytes", maxExtractBytes)
		}
	}
	return nil
}

// EqualFoldPrefix 报告 s 是否以 prefix 开头（大小写不敏感比较）。
// 用于解压路径逃逸检测：Windows 路径比较是大小写不敏感语义，前缀判断若用
// 区分大小写的 HasPrefix，dst 与 entry 以不同大小写拼写时检测会失效
// （与 ensureWithinDir 的 EqualFold 语义对齐）。
func EqualFoldPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(prefix, s[:len(prefix)])
}
