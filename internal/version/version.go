// Package version holds the dynamic build version of the proxy.
//
// Version 的格式为 YYYYMMDD.BUILD（例如 20260618.42），其中：
//   - 日期部分：构建当天的年月日
//   - BUILD 部分：构建序号（CI 的 run_number / git 提交计数 / BUILD_NUMBER 环境变量）
//
// 该值在构建期通过 ldflags 注入：
//
//	go build -ldflags "-X github.com/LeXwDeX/LLM-Proxy/internal/version.Version=20260618.42"
//
// 未注入时（如 go run / 未带 ldflags 的 go build）保持默认值 "dev"，
// 登录页与管理台会显示该占位字符串。
package version

// Version 是当前的构建版本号。构建期由 ldflags 覆盖，默认 "dev"。
var Version = "dev"
