// rollback.go 实现 upgrade 失败时的 .previous 恢复逻辑。
//
// 对称 Linux apply-pending-upgrade.sh 的 maybe_rollback 函数：
//   - 遍历指定目录（递归），找 *.previous 文件
//   - rollback: rename(dest.previous, dest) 恢复旧版本
//   - cleanup: delete *.previous（upgrade 确认健康后）
//
// 错误语义：Rollback 单文件恢复失败不阻断其他文件恢复，但必须
// 使整体返回 error（调用方 RollbackAndMark 据此不写 rollback.done 哨兵，半回滚
// 状态保持可重试）。CleanupPrevious 仍为 best-effort（健康路径清理，失败仅日志）。
//
// context 约定例外：Rollback / CleanupPrevious 操作本地文件系统，
// rollback 必须原子完成（半回滚 = 部分恢复 + 部分未恢复），中途取消不可接受。
// 取消检查由编排层入口完成：BootCheck / superviseWorker 循环顶部 deadline rollback。

package upgrademachine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Rollback 遍历 dirs（递归），将所有 *.previous 文件恢复到原路径。
//
// 例如：worker.exe.previous → worker.exe（原子 rename 替换当前版本）。
// 返回成功恢复的文件数。目录不存在或无 .previous 文件时返回 (0, nil)。
//
// 单文件恢复失败不中断其他文件恢复（能恢复多少恢复多少），
// 但整体返回 error — RollbackAndMark 收到 error 时不写 rollback.done 哨兵，
// 半回滚状态（.previous 未清空）下轮可重试，rename 幂等（已恢复的不再匹配遍历）。
func Rollback(dirs []string) (int, error) {
	return walkPreviousDirs(dirs, "rollback", false, func(prevPath string) error {
		target := strings.TrimSuffix(prevPath, PreviousSuffix)
		return os.Rename(prevPath, target)
	})
}

// CleanupPrevious 删除指定目录（递归）下所有 *.previous 文件。
// 在 upgrade 确认健康后调用（healthy_marker 匹配 last_upgrade_ver）。
// 对称 Linux 的 `find ... -name '*.previous' -delete`。
// best-effort：单文件删除失败跳过，不使整体失败（失败文件残留由日志暴露）。
func CleanupPrevious(dirs []string) (int, error) {
	return walkPreviousDirs(dirs, "cleanup", true, func(prevPath string) error {
		return os.Remove(prevPath)
	})
}

// walkPreviousDirs 对 dirs 中每个目录递归遍历 *.previous 文件，执行 op。
// bestEffort=true 时 op 失败跳过（CleanupPrevious）；
// bestEffort=false 时 op 失败收集为整体 error（Rollback）。
// WalkDir 自身的错误（目录不可访问等）向上传播（与已收集的 op 错误合并）。
func walkPreviousDirs(dirs []string, opName string, bestEffort bool, op func(prevPath string) error) (int, error) {
	total := 0
	var opErrs []error
	for _, dir := range dirs {
		n, err := forEachPrevious(dir, bestEffort, op, &opErrs)
		total += n
		if err != nil {
			opErrs = append(opErrs, fmt.Errorf("%s %s: %w", opName, dir, err))
			break // 目录遍历失败：该目录后续不可达，保留已收集的 op 错误一并返回
		}
	}
	return total, errors.Join(opErrs...)
}

// forEachPrevious 递归遍历 dir，对每个 *.previous 文件调用 fn。
// bestEffort=true 时 fn 失败跳过（best-effort）；
// bestEffort=false 时 fn 失败追加到 opErrs（不中断遍历）。
func forEachPrevious(dir string, bestEffort bool, fn func(prevPath string) error, opErrs *[]error) (int, error) {
	count := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // 目录不存在：合法状态（从未升级过），跳过
			}
			// 其他遍历错误（目录不可访问、权限丢失等）必须向上传播：
			// 吞掉会让 Rollback 返回 (0, nil) 静默成功，调用方据此写入
			// rollback.done 哨兵，半回滚状态从此永久停止恢复。
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), PreviousSuffix) {
			return nil
		}
		if ferr := fn(path); ferr != nil {
			if bestEffort {
				return nil // best-effort：跳过该文件
			}
			*opErrs = append(*opErrs, fmt.Errorf("restore %s: %w", path, ferr))
			return nil // D3：记录失败但继续其他文件，遍历结束后聚合传播
		}
		count++
		return nil
	})
	return count, err
}
