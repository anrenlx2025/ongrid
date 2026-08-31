// machine.go 实现升级状态机深模块。
//
// Machine 将原先分散在 cmd/upgrade_windows.go 的编排逻辑（applyAndSwap、
// maybeApplyOnBoot、maybeRollbackOnBoot、watchUpgradeHealth、rollbackAndMark、
// checkPendingUpgrade）集中到一个类型中。
//
// supervisor 侧（cmd/）通过 NewMachine 创建实例，注入平台专属的 ProcessController，
// 然后调用 4 个高层方法：BootCheck / Apply / HealthPoll / RollbackAndMark。
//
// 纯 Go（无 Windows 专属依赖），测试可在 Linux CI 跑。

package upgrademachine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrApplied 是 sentinel error，CheckPending 返回它告诉 superviseWorker：
// "bundle swap 已完成，跳过 restartDelay 立即重启 worker"。
var ErrApplied = errors.New("upgrade applied; restart immediately")

// DefaultAwaitingHealthMaxAge 是 awaiting_health 哨兵总年龄上限的默认值
// upgradeWatchTimeout（cmd 包，180s）的 5 倍。
// 若 cmd 侧超时窗口调整，此值应保持"数倍"关系（窗口太小会让
// 慢但健康的 worker 被误回滚，太大则崩溃循环收敛慢）。
const DefaultAwaitingHealthMaxAge = 15 * time.Minute

// supervisorDiscardSuffix 是 supervisor 双恢复时运行中新 exe
// 的 rename-aside 后缀：恢复 .old 前必须先把当前 supervisor.exe 挪走（rename
// 运行中 exe 可行，直接替换/删除不可行 — Windows image section 语义）。
// 恢复趟 best-effort 删除；运行中删不掉时下次 BootCheck 步骤 4 前补删。
const supervisorDiscardSuffix = ".discard"

// ProcessController 抽象进程终止操作，供跨平台 DI。
//
// Windows 生产实现用 taskkill；测试传 mock。
type ProcessController interface {
	// KillTree 终止 pid 及其所有子进程（taskkill /T /F /PID）。
	// 进程已退出时应返回非 nil error（调用方忽略，幂等）。
	KillTree(pid int) error

	// KillByImage 按镜像名终止所有同名进程（taskkill /F /IM <name>）。
	// 进程不存在时返回非 nil error（调用方忽略，幂等）。
	KillByImage(name string) error
}

// Machine 是升级状态机深模块，封装 supervisor 侧的升级编排逻辑。
//
// 持有 stageDir（IPC 文件根）和 binDir（swap 目标目录），
// 通过注入的 ProcessController 执行平台专属的进程终止。
type Machine struct {
	stageDir string
	binDir   string
	log      *slog.Logger
	pc       ProcessController
	// selfPathResolver 返回 supervisor 进程实际运行的 .exe 路径。
	// 生产默认 os.Executable（edgedirs.BinDir 硬编码可能与
	// 运行路径不一致，如旧 nssm 遗留的 C:\ongrid-edge\ 安装路径）。
	// 测试注入 stub 模拟非标准安装路径。
	// nil 时 resolveSupervisorPath fallback 到 m.binDir + SupervisorBinaryName。
	selfPathResolver func() (string, error)
	// awaitingHealthMaxAge 是 awaiting_health 哨兵的总年龄上限。
	// 默认 DefaultAwaitingHealthMaxAge；测试注入短值（零新缝：同包直接改字段）。
	awaitingHealthMaxAge time.Duration
	// pendingHealthCheck 由 BootCheck 步骤 1 的 awaiting-health 分支设置：
	// 检测到 supervisor_selfswap_awaiting_health sentinel 时置 true，告诉
	// superviseWorker 初始化 watchUpgrade=true，启 worker 后跑 HealthPoll 180s
	// grace 确认健康（健康判定必须发生在 worker 实际启动后 — BootCheck 阶段
	// worker 尚未启动，此时回滚会在 worker 有机会写健康标记前误杀刚换入的
	// 版本；超时 rollback 由 superviseWorker 循环顶部执行，HealthPoll 本身
	// 只 polling 不持 timer）。
	pendingHealthCheck bool
	// stageDirGuard 可选的 stage 目录安全校验（fail-closed 消费门控）。
	// 升级 bundle 无签名，完整性依赖 stage 目录 ACL；guard 返回 error 时
	// 拒绝消费 pending bundle（不解压、不 apply），但不影响 rollback /
	// self-swap 恢复路径 — 那些输入来自 BinDir（受 Program Files ACL 保护）
	// 而非 stage 目录。由平台调用方注入（Windows 为 ACL 校验；nil = 不设防，
	// 仅限测试或无本地多用户威胁的平台）。
	stageDirGuard func() error
}

// NewMachine 创建升级状态机实例。
//
// 参数：
//   - stageDir: IPC 文件根目录（incoming/、last_upgrade_ver 等在此下）
//   - binDir: swap 目标目录（worker.exe、.previous 文件在此下）
//   - log: 结构化日志
//   - pc: 平台专属进程控制器（nil 时跳过 kill 操作，仅用于 boot 无 worker 场景）
func NewMachine(stageDir, binDir string, log *slog.Logger, pc ProcessController) *Machine {
	return &Machine{
		stageDir:             stageDir,
		binDir:               binDir,
		log:                  log,
		pc:                   pc,
		selfPathResolver:     os.Executable,
		awaitingHealthMaxAge: DefaultAwaitingHealthMaxAge,
	}
}

// resolveSupervisorPath 返回 supervisor.exe 实际运行路径。
// 生产用 os.Executable（NewMachine 默认注入）；测试注入 stub。
// selfPathResolver 为 nil 时 fail-fast（显式失败而非静默降级）—
// NewMachine 已保证注入，nil 说明调用方未走标准构造路径。
func (m *Machine) resolveSupervisorPath() (string, error) {
	if m.selfPathResolver == nil {
		return "", fmt.Errorf("selfPathResolver is nil (NewMachine not used)")
	}
	p, err := m.selfPathResolver()
	if err != nil {
		return "", fmt.Errorf("resolve supervisor self path: %w", err)
	}
	return filepath.Clean(p), nil
}

// BootCheck 是 supervisor 启动时的 boot hook，合并原 maybeRollbackOnBoot + maybeApplyOnBoot
// + supervisor 自升级收尾 / brick 恢复 / self-swap 触发。
//
// 执行顺序（不可反转）：
//  1. 检测上次升级是否健康 → 不健康则 RollbackAndMark；含双恢复（dual
//     recovery）：rollback.done + awaiting_health 共存且 rollback.done 晚于
//     awaiting（mtime 判据，排除 stale 假阳性）= 补 supervisor 侧恢复；
//     awaiting_health 年龄超上限 = 强制双恢复（先 worker 后 supervisor）→
//     SCM 自然重启
//  2. 检测残留 pending upgrade → 有则 Apply（boot 时无 worker，不 kill）
//  3. supervisor_upgrade.applied sentinel → 清理 .old 备份 + 删 sentinel
//  4. brick recovery（supervisor.exe 缺失 + .old 存在）→ rename .old 恢复
//  5. supervisor_upgrade.pending sentinel → KillByImage 清 orphan worker
//     → SupervisorSelfSwap → 返回 ErrSupervisorRestartSoon 让 SCM 重启
//
// 返回最后遇到的错误（如有）。返回 ErrSupervisorRestartSoon 时调用方（service.go）
// 应返回 (false, 1) 让 SCM 按 recovery action 重启（exitCode=0 不触发 restart）。
func (m *Machine) BootCheck(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	var lastErr error

	// 1. rollback.done sentinel → 上次已 rollback，跳过（避免死循环）
	rollbackDone := RollbackDoneExists(m.stageDir)
	awaiting := IsSupervisorSelfSwapAwaitingHealth(m.stageDir)
	if rollbackDone && awaiting && !rollbackDoneAfterAwaitingHealth(m.stageDir) {
		// stale rollback.done（mtime 早于/等于 awaiting_health）是旧周期
		// 回滚残留（DownloadBundle / ConvergeResidualBackups 清理点均在
		// BootCheck 步骤 1 之后，直投 pending 路径先到达这里），与本周期 SelfSwap
		// 刚写的 awaiting_health 同形假阳性 — 只看共存会把健康的刚 selfswap
		// supervisor 误回滚。清残留哨兵后按 awaiting-only 语义继续。
		m.log.Warn("upgrade: stale rollback.done (older than awaiting_health); clearing and treating as selfswap-in-progress")
		if err := os.Remove(RollbackDonePath(m.stageDir)); err != nil && !os.IsNotExist(err) {
			m.log.Warn("upgrade: remove stale rollback.done failed (will retry next boot)", "err", err)
		}
		rollbackDone = false
	}
	if rollbackDone {
		// rollback.done + awaiting_health 共存且 rollback.done 晚于
		// awaiting = superviseWorker 超时路径的中间态 —
		// worker 已回滚、supervisor 侧恢复待执行。保留哨兵不删（superviseWorker）
		// + 本分支补 supervisor 恢复 = 双恢复「先 worker 后 supervisor」顺序；
		// 任意中间点断电后下次 BootCheck 都到达这里（幂等）。
		if awaiting {
			restored, err := m.restoreSupervisorFromOld()
			if err != nil {
				m.log.Error("upgrade: supervisor restore after worker rollback failed", "err", err)
				lastErr = err
				// 不 return — 若 exe 已 rename aside，步骤 4 brick recovery 兜底
			} else if restored {
				return ErrSupervisorRestartSoon // SCM 自然重启加载旧 supervisor
			} else {
				m.log.Warn("upgrade: awaiting_health sentinel present but no .old to restore; clearing stale sentinel")
			}
		} else {
			m.log.Info("upgrade: rollback.done sentinel present; skipping rollback check")
		}
	} else if awaiting {
		if m.awaitingHealthAgeExceeded(time.Now()) {
			// 哨兵总年龄超上限 → 强制双恢复。彻底解决坏
			// supervisor 崩溃循环：每轮 SCM 重启都重新武装 HealthPoll 定时器，
			// 180s deadline 永不到达；哨兵 mtime（= selfswap 武装时刻，无人重写）
			// 是唯一不被重启重置的时钟，超限即判定回滚窗口已彻底耗尽。
			if IsUpgradeHealthy(m.stageDir) {
				// 健康已确认但哨兵残留（HealthPoll 成功路径删除失败的罕见自愈）：
				// 只清残留哨兵，绝不回滚健康升级。
				m.log.Info("upgrade: stale awaiting_health sentinel (age exceeded) but healthy; clearing")
				_ = os.Remove(SupervisorSelfSwapAwaitingHealthPath(m.stageDir))
			} else {
				m.log.Warn("upgrade: awaiting_health sentinel age exceeded; forcing dual rollback")
				// 先 worker 后 supervisor。崩溃循环场景 orphan worker
				// 可能持 worker.exe 文件锁 → 先 KillByImage（对称步骤 5 先例）。
				if m.pc != nil {
					if err := m.pc.KillByImage(WorkerBinaryName); err != nil {
						m.log.Debug("KillByImage returned non-zero (process may not be running)",
							"image", WorkerBinaryName, "err", err)
					}
				}
				if err := m.RollbackAndMark(); err != nil {
					// worker 回滚失败（半回滚可重试）— 仍恢复 supervisor：
					// rollback.done 未写 → 下次 BootCheck 步骤 1 末分支重试 worker 回滚
					m.log.Error("upgrade: worker rollback during forced dual rollback failed", "err", err)
					lastErr = err
				}
				restored, err := m.restoreSupervisorFromOld()
				if err != nil {
					m.log.Error("upgrade: supervisor restore during forced dual rollback failed", "err", err)
					// Join 而非覆盖：worker 回滚失败 + supervisor 恢复失败可能同时发生，
					// 丢前一个 error 会让调用方误判。
					lastErr = errors.Join(lastErr, err)
				} else if restored {
					return ErrSupervisorRestartSoon
				}
			}
		} else {
			// supervisor self-swap 刚完成（上次启动写的 sentinel），
			// worker 还没机会启动写 healthy_marker → 本次启动不 rollback，委托给
			// superviseWorker 的 HealthPoll 180s grace。健康判定必须在 worker
			// 实际启动后：BootCheck 阶段 worker 尚未运行，立即 rollback 会误杀
			// 刚换入的版本（worker 根本没机会证明自己健康）。
			m.pendingHealthCheck = true
			m.log.Info("upgrade: self-swap awaiting health sentinel present; arming HealthPoll")
		}
	} else if HasLastUpgrade(m.stageDir) && !IsUpgradeHealthy(m.stageDir) {
		// 上次升级不健康 → rollback
		m.log.Warn("upgrade: last upgrade not confirmed healthy; rolling back")
		if err := m.RollbackAndMark(); err != nil {
			m.log.Error("upgrade: rollback on boot failed", "err", err)
			lastErr = err
		}
	}

	// 2. 残留 pending → apply（boot 时无 worker，PID 传 0）
	// Windows 兼容：pending tar.gz 可能尚未解压（无 systemd ExecStartPre 对等机制），
	// 与 CheckPending 对称处理。stage 目录安全校验不通过时整段跳过
	// （fail-closed：拒绝消费，rollback / self-swap 恢复不受影响）。
	if guardErr := m.checkStageDir(); guardErr != nil {
		m.log.Error("upgrade: stage dir security check failed; skipping pending apply on boot", "err", guardErr)
		lastErr = guardErr
	} else {
		m.applyPendingOnBoot(ctx, &lastErr)
	}

	// 3. supervisor_upgrade.applied → 清理 .old 备份 + 删 sentinel
	if IsSupervisorUpgradeApplied(m.stageDir) {
		m.log.Info("supervisor self-swap: applied sentinel detected")
		// .old 仅在健康已确认或无待决升级状态（从未升级）
		// 时删除。SelfSwap 写 applied 在健康确认之前 — 无条件删 .old 会让健康判定
		// 失败时 supervisor 永久停留坏新版（.old 是唯一回滚源，brick 兜底也依赖它）。
		// 唯一权威删除点 = HealthPoll 成功路径；此处仅是健康态/无待决态的幂等 backstop。
		if !HasLastUpgrade(m.stageDir) || IsUpgradeHealthy(m.stageDir) {
			m.cleanupSupervisorOld()
		} else {
			m.log.Info("supervisor self-swap: .old preserved until health confirmed")
		}
		// 删 applied 前先求值新周期判据 — pending 严格晚于 applied（mtime）
		// 说明 applied 是上周期残留 + pending 是步骤 2 Apply 本轮刚写，无条件清
		// 会误删新周期 pending → 步骤 5 SelfSwap 不触发 → supervisor.exe.new
		// 永久残留（实机现场：上周期 applied 残留场景）。早于/等于 = 同周期断电孤儿。
		newCyclePending := pendingAfterApplied(m.stageDir)

		// best-effort 删 applied sentinel — 残留下次 BootCheck 会重复清理（幂等）
		_ = os.Remove(SupervisorUpgradeAppliedPath(m.stageDir))
		// applied 已写 = 升级完成权威信号，孤儿 pending 是断电窗口残留
		// （SelfSwap step 3 写 applied 后 + 删 pending 前断电）。不清会导致步骤 5
		// 触发 SelfSwap，但 supervisor.exe 已新版 + .new 不存在 → relocate 失败 →
		// SCM restart 死循环。applied 是真相源时清 pending 是收尾职责的自然延伸。
		// best-effort：删失败下次 BootCheck 步骤 5 会再判 pending（幂等）。
		if newCyclePending {
			m.log.Info("supervisor self-swap: pending sentinel is newer than applied; preserving for self-swap")
		} else {
			_ = os.Remove(SupervisorUpgradePendingPath(m.stageDir))
		}
	}

	// 4. brick recovery: supervisor.exe 缺失 + .old 存在 → rename 恢复
	// 双恢复趟的 .discard aside 残留 best-effort 补删（恢复趟运行中
	// exe 删不掉；正常路径恢复趟内已自清，此处兜底恢复失败的趟次）。
	if supervisorPath, err := m.resolveSupervisorPath(); err == nil {
		if err := os.Remove(supervisorPath + supervisorDiscardSuffix); err == nil {
			m.log.Info("supervisor: removed leftover .discard from dual-restore")
		}
	}
	if m.isSupervisorBrickState() {
		m.log.Warn("supervisor brick state: supervisor.exe missing + .old exists; restoring")
		supervisorPath, err := m.resolveSupervisorPath()
		if err != nil {
			m.log.Error("supervisor brick recovery: resolve self path failed", "err", err)
			lastErr = err
		} else {
			oldPath := supervisorPath + ".old"
			if err := renameWithAVRetry(oldPath, supervisorPath, m.log); err != nil {
				m.log.Error("supervisor brick recovery: restore failed", "err", err)
				lastErr = err
			}
		}
	}

	// 5. supervisor self-swap: pending sentinel → KillByImage → SupervisorSelfSwap
	if IsSupervisorUpgradePending(m.stageDir) {
		// BootCheck 恢复路径可能存在 orphan worker，先清理（幂等）
		if m.pc != nil {
			m.log.Warn("supervisor self-swap on boot: killing orphan worker first",
				"image", WorkerBinaryName)
			if err := m.pc.KillByImage(WorkerBinaryName); err != nil {
				m.log.Debug("KillByImage returned non-zero (process may not be running)",
					"image", WorkerBinaryName, "err", err)
			}
		}
		err := m.SupervisorSelfSwap()
		if errors.Is(err, ErrSupervisorRestartSoon) {
			return err // 让 service.go 触发 SCM restart
		}
		if err != nil {
			m.log.Error("supervisor self-swap on boot failed", "err", err)
			lastErr = err
		}
	}

	return lastErr
}

// restoreSupervisorFromOld 执行 supervisor 侧恢复：
// 运行中的新 supervisor.exe rename aside（.discard）→ .old 改名回 supervisor.exe
// → 清 awaiting_health / applied 哨兵。调用方随后返回 ErrSupervisorRestartSoon
// 让 SCM 自然重启加载旧 supervisor（分钟级延迟可接受，异常路径不追速度）。
//
// 返回 (restored, error)：
//   - .old 不存在：restored=false，err=nil（无事可做；顺带清残留哨兵自愈）
//   - rename 失败：restored=false，err 非 nil（若 exe 已 aside，调用方不 return，
//     BootCheck 步骤 4 brick recovery 兜底 rename .old → exe）
//
// 顺序不可反转：必须先把当前 exe 挪走（rename 运行中 exe 可行），才能把 .old
// rename 回原路径（rename onto 运行中 exe 不可行 — Windows image section）。
// 路径用 resolveSupervisorPath。
func (m *Machine) restoreSupervisorFromOld() (bool, error) {
	supervisorPath, err := m.resolveSupervisorPath()
	if err != nil {
		return false, fmt.Errorf("supervisor restore: %w", err)
	}
	oldPath := supervisorPath + ".old"
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		// 无处恢复 — 清残留哨兵（调用方据 restored=false 走自愈路径）
		_ = os.Remove(SupervisorSelfSwapAwaitingHealthPath(m.stageDir))
		return false, nil
	}

	// 当前 exe（新版，待废弃）rename aside。exe 缺失（brick 态）时跳过 —
	// .old 直接 rename 回原路径即可。
	discardPath := supervisorPath + supervisorDiscardSuffix
	if _, err := os.Stat(supervisorPath); err == nil {
		if err := renameWithAVRetry(supervisorPath, discardPath, m.log); err != nil {
			return false, fmt.Errorf("supervisor restore: rename aside to %s: %w", discardPath, err)
		}
	}

	if err := renameWithAVRetry(oldPath, supervisorPath, m.log); err != nil {
		return false, fmt.Errorf("supervisor restore: rename %s back: %w", oldPath, err)
	}
	m.log.Info("supervisor restore: .old renamed back; supervisor.exe is previous version",
		"restored_from", oldPath)

	// 哨兵清理：awaiting_health（生命周期右端点）+ applied（升级未成立的收尾）。
	// best-effort — 残留下次 BootCheck 幂等清理。
	_ = os.Remove(SupervisorSelfSwapAwaitingHealthPath(m.stageDir))
	_ = os.Remove(SupervisorUpgradeAppliedPath(m.stageDir))
	// aside 的新 exe 已无价值（坏版本）；运行中删不掉（Windows），best-effort。
	_ = os.Remove(discardPath)
	return true, nil
}

// awaitingHealthAgeExceeded 报告 awaiting_health 哨兵的总年龄是否超上限。
// 年龄 = now - 哨兵 mtime；mtime 即 selfswap 武装时刻
// （哨兵仅在 SupervisorSelfSwap step 3 写一次，重启/重新武装定时器不会重写 —
// 这正是崩溃循环中唯一不被重置的时钟）。严格大于语义：恰好等于不触发。
// 哨兵缺失/Stat 失败返回 false（防御：让 awaiting-health 分支按既有语义处理）。
func (m *Machine) awaitingHealthAgeExceeded(now time.Time) bool {
	fi, err := os.Stat(SupervisorSelfSwapAwaitingHealthPath(m.stageDir))
	if err != nil {
		return false
	}
	return now.Sub(fi.ModTime()) > m.awaitingHealthMaxAge
}

// rollbackDoneAfterAwaitingHealth 报告 rollback.done 的 mtime 是否严格晚于
// awaiting_health 的 mtime — 共存分支的真中间态判据。
//
// 真中间态时序：selfswap 武装（写 awaiting_health）→ 健康超时（≥180s grace）→
// worker 回滚（写 rollback.done），故 rollback.done 必晚于 awaiting_health。
// 早于/相等 = stale rollback.done 是旧周期回滚残留（与刚武装的 awaiting_health
// 同形假阳性）。Stat 失败返回 false（防御：宁可漏恢复也不误回滚健康的
// supervisor — 误回滚一个刚换入且健康的版本正是此判据要防的事故）。
func rollbackDoneAfterAwaitingHealth(stageDir string) bool {
	rbFi, err := os.Stat(RollbackDonePath(stageDir))
	if err != nil {
		return false
	}
	ahFi, err := os.Stat(SupervisorSelfSwapAwaitingHealthPath(stageDir))
	if err != nil {
		return false
	}
	return rbFi.ModTime().After(ahFi.ModTime())
}

// pendingAfterApplied 报告 supervisor_upgrade.pending 的 mtime 是否严格晚于
// applied 的 mtime — 步骤 3 清孤儿 pending 前的新周期判据（与
// rollbackDoneAfterAwaitingHealth 同族的 mtime 顺序判据）。
//
// 真孤儿时序：Apply 先写 pending → SelfSwap 后写 applied，故 pending
// 必早于 applied。新周期时序：上周期 SelfSwap 写 applied →
// SCM restart → 步骤 2 Apply 写新 pending（跨 restart ≥ 秒级），故 pending
// 必严格晚于 applied。两集合无交集；相等归孤儿（真孤儿两哨兵至少隔一次
// SelfSwap，不可能相等）。
//
// Stat 失败返回 false（判孤儿，防御方向与 rollbackDoneAfterAwaitingHealth
// 相反但各自正确）：
// 误判新周期（不清）会让真孤儿残留 → 步骤 5 SelfSwap → relocate 失败 →
// SCM restart 死循环（服务不可用）；误判孤儿（清）只损失本轮升级（下轮投喂
// 可恢复）。死循环 > 升级丢失，故偏向维持孤儿清理行为。
func pendingAfterApplied(stageDir string) bool {
	pdFi, err := os.Stat(SupervisorUpgradePendingPath(stageDir))
	if err != nil {
		return false
	}
	apFi, err := os.Stat(SupervisorUpgradeAppliedPath(stageDir))
	if err != nil {
		return false
	}
	return pdFi.ModTime().After(apFi.ModTime())
}

// cleanupSupervisorOld 删除 supervisor.exe.old 备份。
//
// 调用点（唯一两处，均为健康已确认/无待决态）：
//   - HealthPoll 成功路径（权威删除点，与 CleanupPrevious 同点同趟）
//   - BootCheck 步骤 3 applied 分支的幂等 backstop
//
// best-effort：失败仅 Warn — .old 多活一轮无害（下轮 HealthPoll 成功 / BootCheck
// backstop 再试），误删才是不可逆损失。
// 路径用 resolveSupervisorPath。
func (m *Machine) cleanupSupervisorOld() {
	supervisorPath, err := m.resolveSupervisorPath()
	if err != nil {
		m.log.Warn("supervisor self-swap: resolve self path for .old cleanup failed (non-fatal)", "err", err)
		return
	}
	oldPath := supervisorPath + ".old"
	if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
		m.log.Warn("supervisor self-swap: cleanup .old failed (non-fatal)", "err", err)
	}
}

// PendingHealthCheck 报告 BootCheck 是否检测到 supervisor_selfswap_awaiting_health
// sentinel 并要求 superviseWorker 启用 HealthPoll。
//
// superviseWorker 在初始化 watchUpgrade 时调用此 getter：true = 启 worker 后跑
// HealthPoll 180s grace 确认健康（健康判定发生在 worker 实际启动后，避免在
// worker 有机会写健康标记前误回滚）。180s 超时判定移到 superviseWorker 循环
// 顶部，HealthPoll 仅负责 polling。
func (m *Machine) PendingHealthCheck() bool { return m.pendingHealthCheck }

// isSupervisorBrickState 报告 supervisor brick 状态：supervisor.exe 缺失 + .old 存在。
// 此状态发生在 SupervisorSelfSwap step 1 成功（supervisor.exe → .old）+ step 2 失败 +
// brick 兜底也失败 + SCM 重启后。BootCheck 步骤 4 尝试 rename .old 恢复。
//
// 路径用 resolveSupervisorPath。
func (m *Machine) isSupervisorBrickState() bool {
	supervisorPath, err := m.resolveSupervisorPath()
	if err != nil {
		// resolver 失败时不能判断 brick 状态：安全起见返回 false（不触发恢复），
		// 但必须显式 log warn（显式失败而非静默失败）。
		m.log.Warn("supervisor brick check: resolve self path failed (non-fatal)", "err", err)
		return false
	}
	oldPath := supervisorPath + ".old"
	_, supErr := os.Stat(supervisorPath)
	_, oldErr := os.Stat(oldPath)
	return os.IsNotExist(supErr) && oldErr == nil
}

// Apply 编排 bundle swap 的完整顺序：
//  1. KillTree — 释放文件锁（worker 子进程可能持有 .exe 句柄）
//  2. ParseManifest
//     2.5 ValidateAllEntries — 白名单校验（失败零磁盘变动零 kill）
//  3. KillManifestExes — 杀孤儿子进程（windows_exporter 等）
//     3.5 ConvergeResidualBackups — 入口残留收敛
//  4. ApplyBundle — 原子 swap + .previous 备份（失败自恢复）
//  5. WriteUpgradeMeta — 写版本元数据 + 删旧 healthy_marker
//  6. ClearPending — 删 incoming/
//
// workerPID <= 0 时跳过 KillTree（boot 场景 worker 尚未启动）。
//
// ctx 仅用于启动前检查；swap 操作不可中途取消（原子性要求），
// ctx 仅用于启动前检查（boot hooks 调用方可在 swap 前取消）。
func (m *Machine) Apply(ctx context.Context, workerPID int) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// 1. Kill worker tree（释放 .exe 文件锁）
	if workerPID > 0 && m.pc != nil {
		m.log.Info("upgrade: killing worker tree before swap", "pid", workerPID)
		if err := m.pc.KillTree(workerPID); err != nil {
			// 进程已退出时 taskkill 返回非零，忽略（幂等）
			m.log.Warn("upgrade: KillTree returned error (process may have exited)",
				"err", err)
		}
	}

	// 2. Parse manifest
	entries, err := ParseManifest(ManifestPath(m.stageDir))
	if err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}

	// 2.5 白名单校验：必须在任何 kill/swap 之前 —
	// 恶意 manifest 的 Dest 既驱动文件写入也驱动 KillByImage，
	// 校验失败时零磁盘变动、零进程终止（KillTree 是按 PID 杀 worker 树，
	// 不受 manifest 控制，先于校验执行无旁路面）。
	if err := ValidateAllEntries(ctx, m.binDir, IncomingDir(m.stageDir), entries); err != nil {
		return fmt.Errorf("validate manifest entries: %w", err)
	}

	// 3. Kill plugin processes by image name
	m.KillManifestExes(entries)

	// 3.5 入口残留收敛：上次崩溃轮次的 .previous 残留与合法
	// 备份同形（健康标记假阳性使回滚不可达）→ 先幂等收敛再 apply。必须在
	// ValidateAllEntries 之后（未校验 manifest 不得驱动目录操作）、kill 之后
	// （恢复 rename 需文件锁已释放）。
	restored, err := ConvergeResidualBackups(m.stageDir, m.binDir, entries)
	if err != nil {
		return fmt.Errorf("converge residual backups before apply: %w", err)
	}
	if restored > 0 {
		m.log.Warn("upgrade: converged residual backups from crashed apply", "restored", restored)
	}

	// 4. Apply bundle (atomic swap + backup)
	result, err := ApplyBundle(m.stageDir, IncomingDir(m.stageDir), m.log, entries)
	if err != nil {
		return fmt.Errorf("apply bundle: %w", err)
	}
	m.log.Info("upgrade: bundle applied",
		"swapped", len(result.Swapped), "backed_up", len(result.BackedUp))

	// 5. Write upgrade meta
	ver, err := ReadStagedVersion(m.stageDir)
	if err != nil {
		return fmt.Errorf("read staged version: %w", err)
	}
	if err := WriteUpgradeMeta(m.stageDir, ver); err != nil {
		return fmt.Errorf("write upgrade meta: %w", err)
	}

	// 6. Clear pending
	if err := ClearPending(m.stageDir); err != nil {
		return fmt.Errorf("clear pending: %w", err)
	}

	m.log.Info("upgrade: meta written + pending cleared", "version", ver)
	return nil
}

// checkStageDir 执行 stage 目录安全校验（stageDirGuard 为 nil 时恒通过 —
// 仅限无本地多用户威胁的平台或测试场景）。
func (m *Machine) checkStageDir() error {
	if m.stageDirGuard == nil {
		return nil
	}
	return m.stageDirGuard()
}

// SetStageDirGuard 注入 stage 目录安全校验（平台 ACL 检查等）。
// 见 Machine.stageDirGuard 字段注释。须在首次 BootCheck / CheckPending 前调用。
func (m *Machine) SetStageDirGuard(guard func() error) {
	m.stageDirGuard = guard
}

// applyPendingOnBoot 是 BootCheck 步骤 2 的实现：解压未解压的 pending tar.gz
// 并 apply（boot 时无 worker，PID 传 0）。错误记入 lastErr 不中断后续步骤。
func (m *Machine) applyPendingOnBoot(ctx context.Context, lastErr *error) {
	if !IsPending(m.stageDir) && HasPendingBundle(m.stageDir) {
		m.log.Info("upgrade: pending tar.gz detected on boot; extracting to incoming/")
		if err := ExtractPendingBundle(m.stageDir); err != nil {
			m.log.Error("upgrade: extract pending bundle on boot failed", "err", err)
			*lastErr = err
		}
	}
	if IsPending(m.stageDir) {
		m.log.Info("upgrade: pending bundle detected on boot; applying")
		if err := m.Apply(ctx, 0); err != nil {
			m.log.Error("upgrade: apply on boot failed", "err", err)
			*lastErr = err
		}
	}
}

// CheckPending 在 worker 退出后检查是否有 pending upgrade，有则 apply。
//
// 返回 ErrApplied 表示 swap 成功（调用方 superviseWorker 应跳过 restartDelay）。
// 返回其他 error 表示 swap 失败（调用方按普通崩溃处理）。
// 返回 nil 表示无 pending upgrade（调用方按普通崩溃重启）。
//
// Windows 兼容：worker agent_upgrade RPC 下载 bundle
// 到 {stageDir}/pending（tar.gz），Linux 由 systemd ExecStartPre 脚本解压到
// incoming/；Windows 无对等机制，这里自动检测 pending tar.gz 并解压。
func (m *Machine) CheckPending(ctx context.Context, workerPID int) error {
	// fail-closed 消费门控：stage 目录安全校验不通过时拒绝消费 pending bundle。
	// 返回 nil（视同无 pending）而非 error — 升级通道关闭但 worker 监控不受影响，
	// 避免 ACL 异常演变成服务崩溃循环；每次 worker 退出都会重查（自愈 + 告警可见）。
	if guardErr := m.checkStageDir(); guardErr != nil {
		m.log.Error("upgrade: stage dir security check failed; refusing to consume pending bundle", "err", guardErr)
		return nil
	}
	if !IsPending(m.stageDir) {
		// Windows: pending tar.gz 未解压 → 先解压到 incoming/
		if !HasPendingBundle(m.stageDir) {
			return nil
		}
		m.log.Info("upgrade: pending tar.gz detected; extracting to incoming/")
		if err := ExtractPendingBundle(m.stageDir); err != nil {
			m.log.Error("upgrade: extract pending bundle failed", "err", err)
			return err
		}
		if !IsPending(m.stageDir) {
			m.log.Error("upgrade: pending extracted but MANIFEST.txt missing")
			return fmt.Errorf("extract pending: no MANIFEST.txt after extraction")
		}
	}
	m.log.Info("upgrade: pending bundle detected after worker exit; applying swap")
	if err := m.Apply(ctx, workerPID); err != nil {
		m.log.Error("upgrade: Apply failed", "err", err)
		return err
	}
	return ErrApplied
}

// HealthPoll 在新 worker 启动后监控 healthy_marker，确认升级成功。
//
// 此方法阻塞，直到以下之一发生：
//
//   - IsUpgradeHealthy = true → CleanupPrevious → 清 awaiting_health sentinel → 返回 nil（成功）
//   - workerCtx 取消（worker 提前退出或 supervisor 停止）→ 返回 workerCtx.Err
//
// 设计要点：HealthPoll 仅 polling（无 timer / 无 RollbackAndMark）。
// 原因：goroutine 内持 timer 到期调 RollbackAndMark 会撞
// Windows image section 文件锁（运行中的 worker.exe 不可 rename），且 timer
// 分支在 worker 崩溃连锁取消 workerCtx 时永不触发（timer 与 ctx.Done 竞争，
// ctx 总是先到）。故 180s 超时判定 + RollbackAndMark 放在 superviseWorker 循环
// 顶部（worker 已退出空窗 = 文件锁释放），见 worker.go superviseWorker。
//
// pollInterval 是轮询 IsUpgradeHealthy 的间隔（测试可传短值）。
func (m *Machine) HealthPoll(ctx context.Context, pollInterval time.Duration) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if IsUpgradeHealthy(m.stageDir) {
				m.log.Info("upgrade: confirmed healthy; cleaning up .previous")
				n, err := CleanupPrevious([]string{m.binDir})
				if err != nil {
					m.log.Error("upgrade: cleanup failed", "err", err)
				} else {
					m.log.Info("upgrade: cleaned up .previous files", "count", n)
				}
				// .old 权威删除点 — 健康已确认，与 worker 备份清理
				// 同点同趟（best-effort，失败下轮 BootCheck backstop 再试）。
				m.cleanupSupervisorOld()
				// 升级确认 → 清 awaiting_health sentinel（成功路径
				// 右端点）。超时 rollback 路径的清理移到 superviseWorker。
				_ = os.Remove(SupervisorSelfSwapAwaitingHealthPath(m.stageDir))
				return nil
			}
		}
	}
}

// RollbackAndMark 执行 rollback 并写 rollback.done 哨兵。
// 被 BootCheck（启动时不健康）和 superviseWorker 循环顶部（HealthPoll 窗口超时）
// 共用。
func (m *Machine) RollbackAndMark() error {
	n, err := Rollback([]string{m.binDir})
	if err != nil {
		m.log.Error("upgrade: rollback failed", "err", err)
		return err
	}
	m.log.Info("upgrade: rolled back files", "count", n)

	if err := WriteRollbackDone(m.stageDir); err != nil {
		m.log.Error("upgrade: failed to write rollback.done sentinel", "err", err)
		// sentinel 写失败不阻断 — 下次启动可能再次 rollback（best-effort）
	}
	return nil
}

// KillManifestExes 遍历 MANIFEST 条目，对白名单内的 .exe dest 用 KillByImage 杀进程。
//
// 解决场景：worker 干净退出后子进程（windows_exporter.exe 等）被
// orphaned（reparented to PID 1），KillTree 无法触达。
// 这些孤儿进程持有 .exe 文件锁，导致 ApplyBundle 的 rename 失败。
// 幂等：进程不存在时 KillByImage 返回非 nil，忽略。
//
// 白名单过滤：kill 目标必须 ∈ Dest 白名单同一集合。
// kill 按 basename 全局匹配，与 dest 是否落盘无关，不套白名单 = 任意进程杀
// 旁路（manifest 写 BinDir 外的 svchost.exe 即可杀系统进程）。
func (m *Machine) KillManifestExes(entries []ManifestEntry) {
	if m.pc == nil {
		return
	}
	seen := make(map[string]bool)
	for _, e := range entries {
		name := destBase(e.Dest)
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		// 白名单过滤：与 validateDest 同一集合、同一 EqualFold 语义
		if !destBaseAllowed(name) {
			m.log.Warn("upgrade: refusing to kill process outside dest whitelist", "image", name)
			continue
		}
		// 跳过 supervisor 自己：supervisor binary 在 MANIFEST 里用于 rename-aside
		// 自升级，不能 kill 自己；SupervisorSelfSwap 在 superviseWorker
		// 里单独处理。不跳过会导致 supervisor 自杀 → SCM restart 死循环。
		// EqualFold：与 isSupervisorBinary / 白名单比较语义全链路统一。
		if strings.EqualFold(name, SupervisorBinaryName) {
			continue
		}
		seen[key] = true
		m.log.Info("upgrade: killing plugin process by image name", "image", name)
		if err := m.pc.KillByImage(name); err != nil {
			m.log.Debug("upgrade: KillByImage returned non-zero (process may not be running)",
				"image", name, "err", err)
		}
	}
}
