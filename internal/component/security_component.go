package component

import (
	"context"
	"fmt"
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/security"
	"github.com/streasure/util/component"
	"github.com/streasure/util/gatewayutil"
	"github.com/streasure/util/tlog"
)

// SecurityComponent 管理白黑名单、WAF、限流、JWT 认证和熔断器等安全组件的生命周期。
type SecurityComponent struct {
	component.BaseComponent

	ctx context.Context

	cfg config.SecurityConfig
	waf config.WAFConfig
	jwt config.JWTAuthConfig

	// 资源字段包内生命周期管理（Start/Destroy 用），Init 末尾同步写入
	// 注入的 *Resources 发布给 Gateway，不作为外部通路。
	whitelistBlacklist *security.WhitelistBlacklist
	wafInstance        *security.WAF
	rateLimiter        *security.RateLimiter
	jwtAuth            *security.JWTAuthFilter
	circuitBreakerMgr  *security.CircuitBreakerManager

	res *Resources
}

// NewSecurityComponent 创建安全组件：配置与资源容器由组合根（main）显式传入。
func NewSecurityComponent(cfg *config.Config, res *Resources) *SecurityComponent {
	return &SecurityComponent{
		ctx: context.Background(),
		cfg: cfg.Security,
		waf: cfg.WAF,
		jwt: cfg.JWTAuth,
		res: res,
	}
}

func (c *SecurityComponent) Name() string { return "security" }
func (c *SecurityComponent) Order() int   { return 100 }

func (c *SecurityComponent) Init() error {
	tlog.Info(c.ctx, "security component init")

	// 白名单/黑名单与熔断器按启动配置创建（与 WAF/JWT/限流一致：
	// 组件创建与否由 Enabled 决定，热更新仅调整已创建组件的参数）。
	// 未启用时不创建，安全链热路径判空即跳过，快速路径判定（pipeline）才可能生效。
	if c.cfg.Enabled {
		c.whitelistBlacklist = security.NewWhitelistBlacklist()
		for _, ip := range c.cfg.Whitelist {
			c.whitelistBlacklist.AddToWhitelist(ip)
		}
		for _, ip := range c.cfg.Blacklist {
			c.whitelistBlacklist.AddToBlacklist(ip)
		}
	}
	if c.cfg.CircuitBreaker.Enabled {
		c.circuitBreakerMgr = security.NewCircuitBreakerManager()
	}

	// JWT 认证。
	if c.jwt.Enabled {
		if c.jwt.Secret == "" {
			// fail-closed：空 secret意味着 HS256 空键可伪造任意令牌
			return fmt.Errorf("jwtAuth.enabled=true requires jwtAuth.secret")
		}
		c.jwtAuth = security.NewJWTAuthFilter(c.jwt)
		c.res.FC.AddFilter(c.jwtAuth)
	}

	// 限流器。
	if c.cfg.RateLimit.Enabled {
		refresh := time.Second
		if d := gatewayutil.ParseDurationDefault(c.cfg.RateLimit.TokenRefresh, 0); d > 0 {
			refresh = d
		}
		tokens := c.cfg.RateLimit.MaxTokens
		if tokens <= 0 {
			tokens = config.DefaultRateLimitMaxTokens
		}
		c.rateLimiter = security.NewRateLimiter(tokens, refresh)
		if c.cfg.RateLimit.RouteMaxTokens > 0 {
			c.rateLimiter.SetRouteMaxTokens(c.cfg.RateLimit.RouteMaxTokens)
		}
		if c.cfg.RateLimit.GlobalMaxTokens > 0 {
			c.rateLimiter.SetGlobalMaxTokens(c.cfg.RateLimit.GlobalMaxTokens)
		}
	}

	// Web 应用防火墙。
	if c.waf.Enabled {
		c.wafInstance = security.NewWAF(c.waf)
	}

	// 发布到注入的资源容器（Gateway Order 最后读取）。
	c.res.WhitelistBlacklist = c.whitelistBlacklist
	c.res.WAF = c.wafInstance
	c.res.RateLimiter = c.rateLimiter
	c.res.JWTAuth = c.jwtAuth
	c.res.CircuitBreakerMgr = c.circuitBreakerMgr
	return nil
}

func (c *SecurityComponent) Start() error {
	tlog.Info(c.ctx, "security component started whitelist=%d blacklist=%d rateLimit=%v waf=%v jwt=%v",
		len(c.cfg.Whitelist),
		len(c.cfg.Blacklist),
		c.cfg.RateLimit.Enabled,
		c.waf.Enabled,
		c.jwt.Enabled)
	return nil
}

func (c *SecurityComponent) Destroy() {
	tlog.Info(c.ctx, "security component destroying")
	if c.rateLimiter != nil {
		c.rateLimiter.Stop()
	}
}
