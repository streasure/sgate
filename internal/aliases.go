//go:build legacy

package gateway

import "github.com/streasure/sgate/internal/config"

// 把所有扩展组件的配置 struct 通过 type alias 暴露到 gateway 包，
// 使各组件文件可直接使用 `JWTAuthConfig` 等短名，而 struct 定义集中在 config 包。
// 这样 yaml 解析与默认值由 config 包统一管理，gateway 仅引用。
type (
	JWTAuthConfig         = config.JWTAuthConfig
	BalancerConfig        = config.BalancerConfig
	CanaryConfig          = config.CanaryConfig
	TrafficMirrorConfig   = config.TrafficMirrorConfig
	OTelTracerConfig      = config.OTelTracerConfig
	ConfigCenterConfig    = config.ConfigCenterConfig
	AlertWebhookConfig    = config.AlertWebhookConfig
	WebhookItemConfig     = config.WebhookItemConfig
	DegradationConfig     = config.DegradationConfig
	DegradationRuleConfig = config.DegradationRuleConfig
	FilterChainConfig     = config.FilterChainConfig
	FilterItemConfig      = config.FilterItemConfig
)
