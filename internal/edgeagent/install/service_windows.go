//go:build windows

package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/ongridio/ongrid/internal/edgeagent/edgedirs"
)

// SCServiceController 是 ServiceController 的 Windows 实现，
// 通过 sc.exe 管理 Windows Service 生命周期。
type SCServiceController struct {
	name string
}

// NewServiceController 创建 sc.exe ServiceController。
// serviceName 是 Windows Service 名称（如 "ongrid-edge"）。
func NewServiceController(serviceName string) ServiceController {
	return &SCServiceController{name: serviceName}
}

// scExe 返回 sc.exe 的绝对路径（PATH 劫持防护，见 acl_windows.go 注释）。
func scExe() (string, error) {
	return systemTool("sc.exe")
}

// powerShellExe 返回 powershell.exe 的绝对路径（PATH 劫持防护）。
// powershell.exe 不在 System32 根目录，而在 WindowsPowerShell\v1.0 子目录。
func powerShellExe() (string, error) {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = os.Getenv("windir")
	}
	if root == "" {
		return "", fmt.Errorf("SystemRoot environment variable not set; cannot resolve powershell.exe")
	}
	p := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("powershell.exe not found at %s: %w", p, err)
	}
	return p, nil
}

// Create 注册 Windows 服务（sc.exe create）。
func (sc *SCServiceController) Create(binPath string) error {
	exe, err := scExe()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "create", sc.name,
		"binPath=", binPath,
		"start=", "auto",
		"DisplayName=", "Ongrid Edge Agent",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sc.exe create: %w (output: %s)", err, out)
	}
	return nil
}

// ConfigureRecovery 配置 SCM failure recovery action。
// reset=86400（24h window），actions=restart/60000×3（3 次 retry，60s 延迟）。
// service.go Execute 返回 (false, 1)（samesession=false + 非 0 exitCode）时 SCM 按此配置重启。
// 3 次/24h 上限后停止，等价于"立即停止"行为。
func (sc *SCServiceController) ConfigureRecovery() error {
	exe, err := scExe()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "failure", sc.name,
		"reset=", "86400",
		"actions=", "restart/60000/restart/60000/restart/60000",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sc.exe failure: %w (output: %s)", err, out)
	}
	return nil
}

// ConfigureDefenderExclusion 为 ongrid 目录添加 Windows Defender exclusion。
// 仅在 Windows Server with Defender 时生效；
// 第三方 AV 或 Defender 已禁用时返回 error，调用方仅 warn 不阻断。
//
// 参数格式：PowerShell Add-MpPreference 的 -ExclusionPath 接受字符串数组，
// 必须用逗号分隔（"-ExclusionPath A,B"），不能重复同名参数
// （"-ExclusionPath A -ExclusionPath B" 会报 ParameterAlreadyBound）。
func (sc *SCServiceController) ConfigureDefenderExclusion() error {
	exe, err := powerShellExe()
	if err != nil {
		return err
	}
	ps := buildDefenderExclusionCmd(edgedirs.BinDir, edgedirs.DataDir)
	out, err := exec.Command(exe, "-NoProfile", "-NonInteractive",
		"-Command", ps).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Add-MpPreference: %w (output: %s)", err, out)
	}
	return nil
}

// buildDefenderExclusionCmd 生成 Add-MpPreference 命令字符串。
// 提取为独立纯函数以便单元测试验证参数格式（防止 ParameterAlreadyBound 回归）。
func buildDefenderExclusionCmd(binDir, dataDir string) string {
	return fmt.Sprintf(
		`Add-MpPreference -ExclusionPath "%s","%s" -ErrorAction SilentlyContinue`,
		binDir, dataDir,
	)
}

// Start 启动已注册的服务（sc.exe start）。
func (sc *SCServiceController) Start() error {
	exe, err := scExe()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "start", sc.name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sc.exe start: %w (output: %s)", err, out)
	}
	return nil
}

// Stop 停止服务（sc.exe stop），仅忽略"服务未启动/不存在"预期错误。
// 其他错误（权限不足、SCM 不可达、命令缺失等）返回给调用方处理。
//
// sc.exe exit codes（Windows SDK）：
//   - 0 = SUCCESS
//   - 1060 = ERROR_SERVICE_DOES_NOT_EXIST（服务未注册）
//   - 1062 = ERROR_SERVICE_NOT_ACTIVE（服务未启动）
func (sc *SCServiceController) Stop() error {
	exe, err := scExe()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "stop", sc.name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code := exitErr.ExitCode()
			if code == 1060 || code == 1062 {
				return nil
			}
		}
		return fmt.Errorf("sc.exe stop: %w (output: %s)", err, out)
	}
	return nil
}

// Delete 删除服务（sc.exe delete）。
func (sc *SCServiceController) Delete() error {
	exe, err := scExe()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "delete", sc.name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sc.exe delete: %w (output: %s)", err, out)
	}
	return nil
}
