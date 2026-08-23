// worker.go 实现 supervisor.exe 对 worker.exe 的启动 + 监控 + 心跳 watchdog
// （仅做"崩溃重启 + 心跳超时 kill 重启"，bundle upgrade 流程由
// upgrademachine 状态机驱动）。
//
// 健康感知走 health.json 文件 IPC：
//   - worker 每 30s 写一次心跳
//   - supervisor 每 30s 读 + 判断超时（90s 阈值，3× 心跳间隔）
//   - 超时 → kill worker → superviseWorker 外层循环重启

//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/ongridio/ongrid/internal/edgeagent/edgedirs"
	"github.com/ongridio/ongrid/internal/edgeagent/supervisorhealth"
	"github.com/ongridio/ongrid/internal/edgeagent/upgrademachine"
)

// 部署路径常量统一在 internal/edgeagent/edgedirs 包，与 cmd/ongrid-edge
// 共享。
//
// 重启间隔是 worker 异常退出后的固定等待时间。
//
//	不做指数退避（YAGNI）；后续如需要再加资源限制 / 指数退避。
const (
	restartDelay      = 5 * time.Second
	workerKillTimeout = 10 * time.Second
)

// workerExe 返回 worker.exe 的绝对路径（edgedirs.BinDir + 文件名）。
func workerExe() string {
	return edgedirs.BinDir + `\` + edgedirs.WorkerBinary
}

// runWorkerOnly 是交互模式（非 Service）入口，直接启动 worker。
// 用于开发调试（RDP 跑 supervisor.exe 看日志）。
func runWorkerOnly(log *slog.Logger) error {
	ctx, cancel := signalInterruptContext()
	defer cancel()
	m := upgrademachine.NewMachine(
		edgedirs.StageDir, edgedirs.BinDir,
		log, &windowsProcessController{},
	)
	return superviseWorker(ctx, log, m)
}

// signalInterruptContext 返回一个在 Ctrl+C 时 cancel 的 ctx。
// 仅用于交互模式（runWorkerOnly）；服务模式由 SCM Stop 回调 cancel。
func signalInterruptContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}

// superviseWorker 是 supervisor 主循环：启动 worker → 监控 → 异常退出则等待
// restartDelay 后重启。ctx 取消时优雅停止 worker 并返回 ctx.Err。
//
// upgrade 集成：
//
//   - errUpgradeApplied → 跳过 restartDelay 立即重启 + 下一轮进入 upgrade watch
//
//   - rollback.done sentinel 存在 → 跳过 upgrade watch（避免死循环）
//
//   - watchDeadline 到期 + worker 已退出（空窗）→ RollbackAndMark（在文件锁
//     释放后的空窗执行，rename 可成功）
//
//   - 超时回滚后若 supervisor 自升级待恢复（awaiting_health 哨兵），
//     保留哨兵 + return ErrSupervisorRestartSoon → SCM 重启 → BootCheck 恢复 .old
//
// 设计要点：rollback 决策放在本函数循环顶部，而非 HealthPoll goroutine 内
// 的 timer 触发。两个原因：
//   - HealthPoll 若持 workerCtx，worker 崩溃连锁取消 workerCtx 时
//     timer.C 分支永不触发 → RollbackAndMark 永不执行（crash loop 场景）。
//   - worker 仍运行时调 RollbackAndMark → os.Rename 撞
//     Windows image section 文件锁。
//
// 移到 superviseWorker 循环顶部（runWorkerOnce 返回 = worker.exe 已退出）后：
// watchDeadline 由 superviseWorker 持有（免疫 workerCtx 连锁取消），且 worker.exe
// 文件锁已释放（rename 可成功）。
//
// shouldRollbackAfterDeadline 是 superviseWorker 循环顶部 rollback 判定的纯函数
// 抽象，抽出主要为可测性 — superviseWorker 本身需要起 worker 子进程
// 无法在 unit test 跑，但 rollback 决策逻辑（4 条件 AND）可以独立验证。返回 true
// 当且仅当：watchUpgrade 已激活 + watchDeadline 已武装 + 当前时刻已过 deadline +
// 升级未确认健康（IsUpgradeHealthy=false）。superviseWorker 在 runWorkerOnce 返回
// （= worker.exe 已退出空窗）时调用此函数判定是否触发 RollbackAndMark。
func shouldRollbackAfterDeadline(stageDir string, watchUpgrade bool, watchDeadline, now time.Time) bool {
	if !watchUpgrade || watchDeadline.IsZero() {
		return false
	}
	if !now.After(watchDeadline) {
		return false
	}
	return !upgrademachine.IsUpgradeHealthy(stageDir)
}

// maxRollbackRetries 是回滚失败告警阈值。连续失败达到此
// 值时发恰好一次终态告警；重试本身不设上限（见 rollbackAlertDecision 注释）。
const maxRollbackRetries = 3

// rollbackAlertDecision 报告回滚失败后是否发终态告警，
// 对称 shouldRollbackAfterDeadline 的纯函数先例：恰好 failures ==
// maxRollbackRetries 时 true（恰好一次，防告警风暴），之前/之后均 false。
//
// 设计裁决：重试在 watch 保留期间**不设
// 上限**——每轮循环顶空窗重试一次 RollbackAndMark（rename 幂等且廉价，AV/EDR
// 持锁通常短暂，锁释放即自愈收敛）。曾评估的两个替代方案均劣：
//   - 达上限停止重试：锁释放后也不自愈（skip 阻止后续 rename 尝试），
//     纯 worker 升级场景（无 awaiting_health 哨兵）无任何机制重启 supervisor
//     → BootCheck 年龄兜底不可达 → 永久振荡静默态
//   - 达上限 exit(1) 换 SCM 重启上下文重试：锁永持时 4min 级 supervisor 重启
//     循环，且 SCM recovery 配置可能有失败上限（服务最终停止，伤害扩大）
//
// 告警上报通道偏差：supervisor 进程无 manager RPC 客户端
// （网络栈属 worker 进程），告警落地为调用点的 log.Error 结构化告警
// （supervisor.log + Windows 事件日志采集链路可达）；未来加通道时决策
// 语义不变，仅换上报动作。
func rollbackAlertDecision(failures int) bool {
	return failures == maxRollbackRetries
}

func superviseWorker(ctx context.Context, log *slog.Logger, m *upgrademachine.Machine) error {
	// BootCheck 检测到 supervisor_selfswap_awaiting_health
	// sentinel 时设 pendingHealthCheck=true → superviseWorker 启用 upgrade watch，
	// 启 worker 后跑 HealthPoll 180s grace 确认健康（健康判定发生在 worker
	// 实际启动后，避免在 worker 有机会写健康标记前误回滚）。
	watchUpgrade := m.PendingHealthCheck()
	// watchDeadline 是 upgrade watch 窗口的截止时刻。零值 = 未武装。
	// 首次进入 watch 模式时武装（now + upgradeWatchTimeout），runWorkerOnce 读取
	// 它在到期时 cancel worker，让本循环顶部在空窗判定 rollback。
	var watchDeadline time.Time
	if watchUpgrade {
		log.Info("upgrade: watch armed from pendingHealthCheck (supervisor self-swap awaiting health)")
	}
	// rollbackFailures 是 RollbackAndMark 连续失败计数。
	// 告警决策见 rollbackAlertDecision；回滚成功、watch 卸载、进入新升级周期
	// （ErrApplied）时复位 — 每个升级窗口的告警预算独立。
	var rollbackFailures int
	for attempt := 0; ; attempt++ {
		// rollback.done sentinel → 上次 rollback 过，本轮不 watch
		if upgrademachine.RollbackDoneExists(edgedirs.StageDir) {
			if watchUpgrade {
				log.Info("upgrade: rollback.done sentinel present; disarming watch")
				rollbackFailures = 0
			}
			watchUpgrade = false
			watchDeadline = time.Time{}
		}

		// 武装 watchDeadline（首次进 watch 模式 / ErrApplied 重启后）
		if watchUpgrade && watchDeadline.IsZero() {
			watchDeadline = time.Now().Add(upgradeWatchTimeout)
			log.Info("upgrade: watch deadline armed", "timeout", upgradeWatchTimeout, "deadline", watchDeadline)
		}

		err := runWorkerOnce(ctx, log, watchUpgrade, watchDeadline, m)

		if errors.Is(err, upgrademachine.ErrApplied) {
			// swap 完成 → 检查是否需要 supervisor 自升级
			//（bundle 含 supervisor.exe 时 applyOne 写 pending sentinel）
			if upgrademachine.IsSupervisorUpgradePending(edgedirs.StageDir) {
				log.Info("supervisor self-swap pending; triggering rename-aside")
				swapErr := m.SupervisorSelfSwap()
				if errors.Is(swapErr, upgrademachine.ErrSupervisorRestartSoon) {
					return swapErr // 让 service.go 触发 SCM restart 加载新 supervisor
				}
				if swapErr != nil {
					log.Error("supervisor self-swap failed (worker continues with current supervisor; HealthPoll will catch version mismatch)",
						"err", swapErr)
				}
			}
			// 立即重启新 worker + 进入 upgrade watch；watchDeadline 下轮重新武装。
			// 新升级周期 → 失败计数复位。
			log.Info("upgrade applied; restarting worker without delay")
			watchDeadline = time.Time{}
			watchUpgrade = true
			rollbackFailures = 0
			continue
		}

		// runWorkerOnce 返回 = worker.exe 已退出
		// 空窗。若 watch 窗口已到期 + 不健康 → 在此处 RollbackAndMark（worker.exe
		// 文件锁已释放，os.Rename 不再撞 image section）。
		//
		// TOCTOU 不变式：shouldRollbackAfterDeadline 内部
		// 读 IsUpgradeHealthy 与下面的 RollbackAndMark 写之间存在理论竞态窗口，
		// 但 worker 已退出（runWorkerOnce 返回的先验条件）= 唯一可能写 healthy_marker
		// 的进程已死 = 无并发 fs 写入 = TOCTOU 不可达。未来若加 manager RPC 远程写
		// healthy_marker 的能力，此不变式需重新评估。
		if shouldRollbackAfterDeadline(edgedirs.StageDir, watchUpgrade, watchDeadline, time.Now()) {
			// 编排顺序约束：终止进程 → 等 worker 退出 → 回滚。
			// 本块处于 runWorkerOnce 返回之后（worker.exe 已退出空窗，文件锁已释放），
			// RollbackAndMark 是纯文件操作（upgrademachine.Rollback 无进程探测）—
			// 「先杀后回滚」的顺序由本编排层保证；BootCheck 强制双恢复分支的
			// KillByImage → RollbackAndMark 顺序由 dualrestore 测试锚定。
			if rerr := m.RollbackAndMark(); rerr != nil {
				// 回滚失败保留 watch 武装与哨兵，循环顶持续重试
				// （不设上限，见 rollbackAlertDecision 注释的方案裁决）。达阈值告警
				// 恰好一次；此后失败日志降频 Debug（每轮空窗一次，Error 会刷屏）。
				rollbackFailures++
				if rollbackAlertDecision(rollbackFailures) {
					log.Error("ALERT: upgrade rollback keeps failing; manual intervention may be required",
						"failures", rollbackFailures, "threshold", maxRollbackRetries, "err", rerr)
				}
				if rollbackFailures <= maxRollbackRetries {
					log.Error("upgrade: rollback after deadline failed; will retry at loop top",
						"err", rerr, "attempt", rollbackFailures)
				} else {
					log.Debug("upgrade: rollback retry still failing (degraded log)",
						"err", rerr, "attempt", rollbackFailures)
				}
			} else if upgrademachine.IsSupervisorSelfSwapAwaitingHealth(edgedirs.StageDir) {
				// 双恢复：worker 已回滚（先），supervisor 侧恢复
				// 走 BootCheck 哨兵路径（后）— 保留 awaiting_health 哨兵不删，
				// 它与 rollback.done 的组合 = "worker 已回滚、supervisor 待恢复"
				// 中间态信号，BootCheck 步骤 1 检测后恢复 .old 并触发 SCM 重启。
				// 断电安全：中间点断电后下次 BootCheck 到达同一恢复路径（幂等）。
				log.Info("upgrade: worker rolled back; supervisor restore pending via BootCheck; exiting for SCM restart")
				return upgrademachine.ErrSupervisorRestartSoon
			} else {
				// 纯 worker 升级超时（bundle 不含 supervisor.exe，无自恢复源）：
				// 卸载 watch 即可。回滚已成功，失败计数复位。
				watchUpgrade = false
				watchDeadline = time.Time{}
				rollbackFailures = 0
			}
		} else if watchUpgrade && !watchDeadline.IsZero() && time.Now().After(watchDeadline) {
			// deadline 到了但 IsUpgradeHealthy=true → 升级成功，卸载 watch
			log.Info("upgrade: watch deadline reached; upgrade healthy, disarming watch")
			watchUpgrade = false
			watchDeadline = time.Time{}
			rollbackFailures = 0
		}

		if err != nil && ctx.Err() == nil {
			log.Error("worker exited unexpectedly", "attempt", attempt, "err", err)
		}

		if ctx.Err() != nil {
			log.Info("supervisor context cancelled; exiting worker loop")
			return ctx.Err()
		}

		log.Info("restarting worker", "after", restartDelay, "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(restartDelay):
		}
	}
}

// runWorkerOnce 启动一次 worker.exe，阻塞等待其退出。
// 退出原因有三：(1) worker 自己崩溃（Wait 返回 err）；(2) watchdog 发现心跳
// 超时，cancel workerCtx → kill worker；(3) 父 ctx 取消（服务停止）。
//
// watchUpgrade=true 时，worker 启动后额外启动 upgrade watch goroutine
// （HealthPoll polling healthy_marker，  去掉原 timer 分支）。
// watchDeadline 到期时本函数 cancel worker，让 superviseWorker 循环顶部在
// 空窗判定 rollback（worker.exe 文件锁已释放）。
//
// worker 退出后检测 pending upgrade（checkPendingUpgrade），有则 swap 并返回
// errUpgradeApplied sentinel 让 superviseWorker 跳过 restartDelay。
func runWorkerOnce(ctx context.Context, log *slog.Logger, watchUpgrade bool, watchDeadline time.Time, m *upgrademachine.Machine) error {
	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()

	// CommandContext 而非 exec.Command：Go 1.20+ 要求设了 cmd.Cancel 的命令
	// 必须用 CommandContext 创建，否则 Start 返回
	// "exec: command with a non-nil Cancel was not created with CommandContext"。
	cmd := exec.CommandContext(workerCtx, workerExe())
	// Explicitly construct worker env to dodge a Windows SCM corner case:
	// service Environment registry field is injected into the service's
	// own process env block, but worker subprocesses spawned via
	// exec.Command (CreateProcess with NULL lpEnvironment) inherit the
	// *current process* env — which on some Windows builds excludes
	// service-specific pairs (root cause TBD; symptom: worker's
	// ONGRID_EDGE_ACCESS_KEY comes through empty even though the field
	// is set in HKLM\...\Services\ongrid-edge\Environment). Workaround:
	// read the Environment multi-string straight from the registry and
	// merge with os.Environ so the worker sees every pair the operator
	// configured via supervisor.exe --install. .
	cmd.Env = mergedServiceEnv()
	// worker stdout/stderr 重定向到轮转日志文件（supervisor.log 已有自己的 sink）。
	// Windows Service 进程无 console，nil inherit = 丢弃；改用 append-only file
	// 让 worker 的 slog 输出可观测。
	if workerStdout, err := openWorkerLog("worker-stdout.log"); err == nil {
		cmd.Stdout = workerStdout
		defer workerStdout.Close()
	} else {
		log.Warn("supervisor: open worker stdout log failed; falling back to discard", "err", err)
		cmd.Stdout = nil
	}
	if workerStderr, err := openWorkerLog("worker-stderr.log"); err == nil {
		cmd.Stderr = workerStderr
		defer workerStderr.Close()
	} else {
		log.Warn("supervisor: open worker stderr log failed; falling back to discard", "err", err)
		cmd.Stderr = nil
	}

	// Go 1.20+ CommandContext 自动 kill 进程当 ctx 取消；指定 Cancel 用优雅方式
	// （taskkill /T 先 send Ctrl-Break，再 workerKillTimeout 后强制 kill）。
	cmd.Cancel = func() error {
		// taskkill /T /F 等于 SIGKILL 整个进程树（绝对路径防 PATH 劫持，
		// 见 upgrade_windows.go taskkillExe）。
		// 不做优雅停止（worker 收到 TerminateProcess 直接退出，状态由
		// health.json + 启动时从 manager 重连 tunnel 恢复）。
		exe, err := taskkillExe()
		if err != nil {
			return err
		}
		return exec.Command(exe, "/T", "/F", "/PID", fmt.Sprint(cmd.Process.Pid)).Run()
	}
	cmd.WaitDelay = workerKillTimeout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start worker %q: %w", workerExe(), err)
	}

	log.Info("worker started", "pid", cmd.Process.Pid, "path", workerExe())

	workerPID := cmd.Process.Pid
	wdErr := make(chan error, 1)
	go func() {
		wdErr <- watchHeartbeat(workerCtx, workerPID, log)
	}()

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cmd.Wait()
	}()

	// upgrade watch goroutine（仅 swap 后首次启动时活跃）。
	// HealthPoll 仅 polling，无 timer / 无 RollbackAndMark —
	// 超时判定移到 superviseWorker 循环顶部。
	var uwErr chan error
	if watchUpgrade {
		uwErr = make(chan error, 1)
		go func() {
			uwErr <- m.HealthPoll(workerCtx, upgradePollInterval)
		}()
		log.Info("upgrade: HealthPoll activated", "poll_interval", upgradePollInterval, "deadline", watchDeadline)
	}

	// deadlineCh：watchDeadline 到期时触发 cancel workerCtx，让 waitErr 路径返回。
	// superviseWorker 下一轮循环顶部在空窗（worker.exe 已退出）判定 rollback。
	// 这是核心时序：deadline 由 superviseWorker 持有（免疫 workerCtx
	// 连锁取消），rollback 在 worker 已退出时执行（无文件锁）。
	//
	// remaining<=0 边界：superviseWorker 重启 worker 时若
	// watchDeadline 已过期（如 watchdog 触发 runWorkerOnce 返回耗时超过剩余窗口），
	// 不能让 worker 无限运行等 watchdog 兜底 — 立即 cancel 让 superviseWorker
	// 顶部 shouldRollbackAfterDeadline 在空窗判定。
	var deadlineCh <-chan time.Time
	deadlineAlreadyExpired := false
	if watchUpgrade && !watchDeadline.IsZero() {
		remaining := time.Until(watchDeadline)
		if remaining > 0 {
			deadlineCh = time.After(remaining)
		} else {
			// deadline 已过期：worker 启动前就超时 → 立即 cancel 让 waitErr 返回。
			// 用 deferred cancel + continue-into-select 会让 worker 启动后立即被 kill。
			// 简化：标记后启 worker 会让 cmd.Start 成功但 cmd.Wait 很快返回（taskkill）。
			deadlineAlreadyExpired = true
		}
	}

	// 边界处理：deadline 在 worker 启动前已过期 → 立即 cancel 让 waitErr 返回。
	// superviseWorker 下一轮循环顶部在空窗判定 rollback。
	if deadlineAlreadyExpired {
		log.Warn("upgrade: watch deadline already expired at worker start; cancelling immediately")
		workerCancel()
	}

	for {
		select {
		case err := <-waitErr:
			// worker 自己挂了。watchdog goroutine 在 workerCtx cancel 后会返回 nil。
			workerCancel()
			<-wdErr
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// worker 退出后检测 pending upgrade
			if uerr := m.CheckPending(ctx, workerPID); uerr != nil {
				return uerr
			}
			if err != nil {
				return fmt.Errorf("worker wait: %w", err)
			}
			return nil

		case err := <-wdErr:
			// watchdog 触发：kill worker → 等 Wait 返回。
			if err != nil {
				log.Error("watchdog killed worker", "reason", err, "pid", workerPID)
			}
			workerCancel()
			<-waitErr
			// watchdog kill 后也检测 pending upgrade
			if uerr := m.CheckPending(ctx, workerPID); uerr != nil {
				return uerr
			}
			return err

		case err := <-uwErr:
			// upgrade watch 结果（只处理一次，然后置 nil 防止重复 select）
			uwErr = nil
			if err == nil {
				// healthy_marker 匹配 → cleanup + 清 sentinel 已完成，继续监控 worker。
				// 健康已确认 → 解除超时定时器（通道引用
				// 置空）。不置 nil 的话，残留定时器在健康确认后到期会误杀一次已确认
				// 健康的 worker（真 bug：watchDeadline 与健康确认是独立时钟）。
				deadlineCh = nil
				continue
			}
			// workerCtx.Err（worker 先退出 / deadlineCh cancel / wdErr cancel）→
			// 交给 waitErr/wdErr 路径处理，这里不返回。
			log.Debug("upgrade HealthPoll ended with ctx error", "err", err)
			continue

		case <-deadlineCh:
			// watchDeadline 到期：cancel worker 让 waitErr 路径返回。superviseWorker
			// 循环顶部在空窗判定 rollback（worker 已退出，文件锁已释放）。本分支只触发一次。
			log.Warn("upgrade: watch deadline fired; cancelling worker for superviseWorker rollback",
				"deadline", watchDeadline)
			deadlineCh = nil
			workerCancel()
			// 循环继续 — waitErr 会因 workerCtx cancel 触发 cmd.Cancel taskkill 而返回。
		}
	}
}

// watchHeartbeat 周期性读 health.json 判断 worker 心跳是否过期。
// 发现过期 → 返回 error（触发外层 kill worker）。
// workerCtx 取消时立即返回 nil（worker 已被外层 kill，不需要再报警）。
//
// startupGrace 是 worker 启动到首次写 health.json 的宽限时间（2× HeartbeatTimeout = 180s），
// 超过此窗口 health.json 还未出现 → 视为 worker 启动失败。
func watchHeartbeat(workerCtx context.Context, workerPID int, log *slog.Logger) error {
	startupGrace := 2 * supervisorhealth.HeartbeatTimeout
	startupDeadline := time.Now().Add(startupGrace)
	ticker := time.NewTicker(supervisorhealth.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-workerCtx.Done():
			return nil
		case <-ticker.C:
			h, err := supervisorhealth.Read(edgedirs.HealthFile)
			if err != nil {
				if supervisorhealth.IsNotExist(err) {
					if time.Now().After(startupDeadline) {
						return fmt.Errorf("worker (pid=%d) started but did not write health.json within %v", workerPID, startupGrace)
					}
					log.Info("health.json not yet written; worker still starting")
					continue
				}
				log.Error("read health.json failed", "err", err)
				continue
			}

			// PID race 检查：health.json 可能是上一个 worker 进程残留。
			if h.WorkerPID != workerPID {
				log.Warn("health.json PID mismatch; ignoring",
					"expected", workerPID, "got", h.WorkerPID)
				continue
			}

			if supervisorhealth.IsStale(h, time.Now(), supervisorhealth.HeartbeatTimeout) {
				return fmt.Errorf("worker (pid=%d) heartbeat stale (last: %s)",
					workerPID, h.LastHeartbeat.Format(time.RFC3339))
			}
		}
	}
}

// openWorkerLog 打开 DataDir 下的 worker 日志文件（append 模式）。
// 用于将 worker stdout/stderr 从 Windows Service nil-sink 救出来。
// 失败时返回 error，调用方降级为 nil（= 丢弃）。
func openWorkerLog(name string) (*os.File, error) {
	path := edgedirs.DataDir + `\` + name
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// mergedServiceEnv returns an env slice suitable for exec.Cmd.Env that
// merges the current process env with values read directly from the
// service's Environment registry field.
//
// Why: Windows SCM injects the service Environment multi-string into the
// service's own process env block at startup, and CreateProcess with a
// NULL lpEnvironment inherits the parent's env. In theory a worker
// subprocess spawned via exec.Command (no explicit Env) should see all
// service-specific pairs. In practice some Windows builds skip the
// injection for child processes whose binary path differs from the
// service's registered BinaryPath (supervisor.exe vs worker.exe), so
// the worker's ONGRID_EDGE_ACCESS_KEY comes through empty even though
// the registry field is set (observed on real Windows Server hosts).
//
// Reading the field directly and merging is the surgical fix that keeps
// operator-facing `supervisor.exe --install` UX unchanged. Failures
// (registry access denied, field absent) silently fall back to
// os.Environ — same behaviour as before this fix.
//
// serviceEnvReader / environFunc are package vars so tests can stub
// them without spinning up a real registry entry.
var (
	serviceEnvReader = readServiceEnvField
	environFunc      = os.Environ
)

func mergedServiceEnv() []string {
	base := environFunc()
	pairs, err := serviceEnvReader()
	if err != nil || len(pairs) == 0 {
		return base
	}
	// Build KEY=value map from base so service pairs can override without
	// producing duplicates. Windows env is case-insensitive — fold keys
	// to upper for the merge but preserve original case in the value.
	merged := make(map[string]string, len(base)+len(pairs))
	order := make([]string, 0, len(base)+len(pairs))
	keyOf := func(kv string) string {
		if k, _, found := strings.Cut(kv, "="); found {
			return strings.ToUpper(k)
		}
		return strings.ToUpper(kv)
	}
	for _, kv := range base {
		k := keyOf(kv)
		if _, ok := merged[k]; !ok {
			order = append(order, k)
		}
		merged[k] = kv
	}
	for _, kv := range pairs {
		k := keyOf(kv)
		if _, ok := merged[k]; !ok {
			order = append(order, k)
		}
		merged[k] = kv
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, merged[k])
	}
	return out
}
