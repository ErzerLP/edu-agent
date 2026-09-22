package blackbox

import "testing"

func TestBlackBoxDeviceStatusWithCompatibleModel(t *testing.T) {
	h := newHarness(t)
	home := h.newCLIHome("模型能力状态")
	code := h.createPairingCodeForProfile("user")
	paired := h.runCLI(home, code+"\n", "pair", "--server", h.serverURL, "--name", "模型能力验收")
	requireExit(t, paired, 0, "普通用户档案配对")

	for _, phase := range []string{"首次查询", "重复查询"} {
		t.Run(phase, func(t *testing.T) {
			result := h.runCLI(home, "", "device", "status")
			requireExit(t, result, 0, "兼容模型设备状态")
			for _, marker := range []string{"Reachability: reachable", "Readiness:", "Device: 模型能力验收", "Device ID:", "Scopes:", "model:probe", "Model: compatible"} {
				requireContains(t, result.stdout, marker, "设备与模型状态输出")
			}
		})
	}
}
