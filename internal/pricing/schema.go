package pricing

import "github.com/LeXwDeX/LLM-Proxy/internal/config"

var modeOptions = []config.EnumOption{
	{Value: ModeFlat, Label: "全天"},
	{Value: ModeSchedule, Label: "分时段"},
}

// AllModeOptions exposes the same modes that template validation accepts.
func AllModeOptions() []config.EnumOption {
	return append([]config.EnumOption(nil), modeOptions...)
}

func IsValidMode(mode string) bool {
	for _, option := range modeOptions {
		if option.Value == mode {
			return true
		}
	}
	return false
}
