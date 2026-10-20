package proxy

import (
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// applyPoolPause 物化池暂停：成员 target.Paused |= 所属池.Paused。
//
// 池暂停通过现有 Paused 读取点生效（目标选择拒绝、粘连拒绝、状态展示），
// 选择层零改动。不修改入参切片（ApplyConfig 的 cfg 由调用方持有）；
// 池不存在的引用视为未暂停。
func applyPoolPause(targets []config.Target, pools []config.DeploymentPool) []config.Target {
	paused := make(map[string]struct{}, len(pools))
	for _, p := range pools {
		if !p.Paused {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(p.Name))
		if name != "" {
			paused[name] = struct{}{}
		}
	}
	if len(paused) == 0 {
		return targets
	}

	out := make([]config.Target, len(targets))
	copy(out, targets)
	for i := range out {
		pool := strings.ToLower(strings.TrimSpace(out[i].Pool))
		if pool == "" {
			continue
		}
		if _, ok := paused[pool]; ok {
			out[i].Paused = true
		}
	}
	return out
}
