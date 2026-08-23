// apply.go 实现 bundle swap 算法。
//
// 对称 Linux apply-pending-upgrade.sh 的 bundle apply 模式：
//  1. 预检：验证所有 src 的 sha256（all-or-nothing，半换比不换更糟）
//  2. 对每条 entry：
//     a. 如果 dest 存在 → copy(dest, dest.previous)（备份）
//     b. copy(src, dest.new)（暂存到 dest 同目录，保证同卷原子 rename）
//     c. rename(dest.new, dest)（原子替换）
//
// Windows 特殊考虑：
//   - os.Rename 在同目录下是原子的（底层 MoveFileExW + MOVEFILE_REPLACE_EXISTING）
//   - copy 到 dest.new 而非直接 rename src→dest，因为 incoming/ 和 dest 可能跨卷
//   - 残留 dest.new（上次崩溃）在步骤 b 被覆盖，幂等
//
// context 约定例外：ApplyBundle 操作本地文件系统（秒级），
// 是原子事务语义（all-or-nothing），中途取消比跑完更危险（半 swap 状态）。
// 取消检查由编排层 Machine.Apply 在入口处完成（ctx.Err guard），
// 进入 ApplyBundle 后操作不可取消。故不接收 context.Context 参数。

package upgrademachine

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// ApplyResult 记录 swap 结果，供调用方决策（日志 + 是否触发 rollback）。
type ApplyResult struct {
	Swapped  []string // 成功 swap 的 dest 路径
	BackedUp []string // 创建的 .previous 备份路径
	Staged   []string // supervisor.exe.new 暂存路径（未 rename，等 SupervisorSelfSwap）
}

// ApplyBundle 执行 MANIFEST 中所有条目的 swap。
//
// 预检阶段（VerifyAll）失败时立即返回，不触碰任何 dest 文件。
// swap 阶段中单条 entry 失败时执行失败自恢复：对本次已备份的
// 全部条目（BackedUp 集合，含未及改名的）逆序恢复 + 清理 supervisor 暂存文件
// 与升级哨兵，然后返回 error。恢复集必须含未换条目 — 残留 .previous 与合法
// 备份同形，会阻断/污染下次 apply。
//
// stageDir 用于写 supervisor_upgrade.pending sentinel。
// 当 entry.Dest 是 supervisor.exe 时，只 stage .new + 写 sentinel，不 rename。
// log 用于 renameWithAVRetry 的 AV 重试日志（nil 时重试静默，仍重试）。
func ApplyBundle(stageDir, incomingDir string, log *slog.Logger, entries []ManifestEntry) (*ApplyResult, error) {
	// 预检：所有 src 的 sha 必须验证通过
	if err := VerifyAll(incomingDir, entries); err != nil {
		return nil, fmt.Errorf("pre-verify aborted: %w", err)
	}

	result := &ApplyResult{}
	for _, e := range entries {
		if err := applyOne(stageDir, incomingDir, log, e, result); err != nil {
			recErr := recoverFailedApply(stageDir, result)
			if recErr != nil {
				return result, fmt.Errorf("swap %s: %w (recovery failed: %w)", e.Src, err, recErr)
			}
			return result, fmt.Errorf("swap %s: %w (recovered %d backups, supervisor staging cleaned)",
				e.Src, err, len(result.BackedUp))
		}
	}
	return result, nil
}

// recoverFailedApply 是 apply 失败自恢复。
//
// 幂等性：BackedUp 条目 rename 回原路径 — 已改名条目 = 恢复旧版本，未及改名
// 条目 = 同内容原地替换；rename 语义与 Rollback 一致（同目录原子替换）。
// 恢复失败不静默：返回 error（调用方把它与原始 swap 错误一起传播，残留态由
// 下次 apply 入口的收敛兜底）。
func recoverFailedApply(stageDir string, result *ApplyResult) error {
	var errs []error
	// 逆序恢复已备份全集（含未及改名条目 — 它们的 .previous 同样是残留物）
	for i := len(result.BackedUp) - 1; i >= 0; i-- {
		prevPath := result.BackedUp[i]
		target := strings.TrimSuffix(prevPath, PreviousSuffix)
		if err := os.Rename(prevPath, target); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", prevPath, err))
		}
	}
	// 清理 supervisor 暂存 .new + pending 哨兵 — 失败 bundle 不得在下次启动
	// 越权触发 supervisor self-swap（swap 的是本轮 stage 的 .new，非受控状态）
	if err := cleanupSupervisorStaging(stageDir, result.Staged...); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// isSupervisorBinary 判断 dest 是否指向 supervisor.exe。
// applyOne 用此决定走 stage-only 路径还是标准 swap 路径。
// EqualFold：与 KillManifestExes / destBaseAllowed 比较语义统一 —
// 区分大小写比较会被大小写变体绕过 supervisor 保护（走标准 swap 路径撞 image 锁）。
func isSupervisorBinary(dest string) bool {
	return strings.EqualFold(destBase(dest), SupervisorBinaryName)
}

// applyOne 执行单条 entry 的 swap：backup → stage → atomic rename。
//
// supervisor.exe special-case：运行中的 supervisor.exe 无法被
// 原子 rename（image loader 持有 image section）。改为只 stage .new + 写
// pending sentinel，让 Machine.SupervisorSelfSwap 后续做 rename-aside。
func applyOne(stageDir, incomingDir string, log *slog.Logger, e ManifestEntry, result *ApplyResult) error {
	srcPath := filepath.Join(incomingDir, e.Src)
	destDir := filepath.Dir(e.Dest)

	// 确保 dest 父目录存在（首次安装路径可能不存在）
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("mkdir dest dir %s: %w", destDir, err)
	}

	// supervisor.exe special-case：stage .new + 写 pending sentinel，不 backup / rename
	if isSupervisorBinary(e.Dest) {
		newPath := e.Dest + ".new"
		if err := copyFile(srcPath, newPath); err != nil {
			return fmt.Errorf("supervisor stage to %s: %w", newPath, err)
		}
		if e.Mode != "" {
			_ = os.Chmod(newPath, parseMode(e.Mode))
		}
		if err := WriteSupervisorUpgradePending(stageDir, ""); err != nil {
			return fmt.Errorf("write supervisor pending sentinel: %w", err)
		}
		result.Staged = append(result.Staged, newPath)
		return nil
	}

	// 备份：dest 存在 → copy(dest, dest.previous)
	if _, err := os.Stat(e.Dest); err == nil {
		prevPath := e.Dest + PreviousSuffix
		if err := copyFile(e.Dest, prevPath); err != nil {
			return fmt.Errorf("backup to %s: %w", prevPath, err)
		}
		result.BackedUp = append(result.BackedUp, prevPath)
	}

	// 暂存：copy(src, dest.new) — 同目录保证后续 rename 原子
	newPath := e.Dest + ".new"
	if err := copyFile(srcPath, newPath); err != nil {
		return fmt.Errorf("stage to %s: %w", newPath, err)
	}

	// 应用 mode（Windows 上仅 read-only bit 有效，但保持语义一致）
	if e.Mode != "" {
		_ = os.Chmod(newPath, parseMode(e.Mode)) // 失败不阻断（Windows 忽略）
	}

	// 原子替换：rename(dest.new, dest) — 同目录 = 同卷，原子。
	// 走 renameWithAVRetry（与 SupervisorSelfSwap 同一容错标准）：KillTree/
	// KillByImage 后 AV/EDR 可能仍短暂持有 dest 句柄，裸 rename 会瞬时失败。
	if err := renameWithAVRetry(newPath, e.Dest, log); err != nil {
		_ = os.Remove(newPath) // best-effort 清理暂存文件；rename 已失败，清理失败也无处理手段
		return fmt.Errorf("rename %s → %s: %w", newPath, e.Dest, err)
	}

	result.Swapped = append(result.Swapped, e.Dest)
	return nil
}

// copyFile 是 src → dst 的完整拷贝（内容 + 权限）。
// 不用 os.Rename 是因为 src 和 dst 可能跨卷（incoming/ 在 ProgramData，
// dest 在 Program Files）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat src: %w", err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copy: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("close dst: %w", err)
	}
	return nil
}

// parseMode 将 "0755" 等字符串转为 os.FileMode。
func parseMode(s string) os.FileMode {
	var m uint32
	for _, c := range s {
		if c >= '0' && c <= '7' {
			m = m<<3 | uint32(c-'0')
		}
	}
	return os.FileMode(m)
}
