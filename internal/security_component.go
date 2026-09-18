package internal

import (
	"context"
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/security"
	"github.com/streasure/sgate/internal/types"
	"github.com/streasure/util/component"
	"github.com/streasure/util/tlog"
)

// SecurityComponent 管理白黑名单、WAF、限流、JWT 认证和熔断器等安全组件的生命周期。
type SecurityComponent struct {
	component.BaseComponent

	cfg config.SecurityConfig
	waf config.WAFConfig
	jwt config.JWTAuthConfig

	WhitelistBlacklist *security.WhitelistBlacklist
	WAF                *security.WAF
	RateLimiter        *security.RateLimiter
	JWTAuth            *security.JWTAuthFilter
	CircuitBreakerMgr  *security.CircuitBreakerManager
	FilterChain        *types.FilterChain
}

func NewSecurityComponent(cfg config.SecurityConfig, wafCfg config.WAFConfig, jwtCfg config.JWTAuthConfig, fc *types.FilterChain) *SecurityComponent {
	return &SecurityComponent{
		cfg:         cfg,
		waf:         wafCfg,
		jwt:         jwtCfg,
		FilterChain: fc,
	}
}

func (c *SecurityComponent) Name() string { return "security" }
func (c *SecurityComponent) Order() int   { return 100 }

func (c *SecurityComponent) Init() error {
	tlog.Info(context.Background(), "security component init")

	c.WhitelistBlacklist = security.NewWhitelistBlacklist()
	c.CircuitBreakerMgr = security.NewCircuitBreakerManager()

	// 白名单和黑名单。
	if c.cfg.Enabled {
		for _, ip := range c.cfg.Whitelist {
			c.WhitelistBlacklist.AddToWhitelist(ip)
		}
		for _, ip := range c.cfg.Blacklist {
			c.WhitelistBlacklist.AddToBlacklist(ip)
		}
	}

	// JWT 认证。
	if c.jwt.Enabled {
		c.JWTAuth = security.NewJWTAuthFilter(c.jwt)
		c.FilterChain.AddFilter(c.JWTAuth)
	}

	// 限流器。
	if c.cfg.RateLimit.Enabled {
		refresh := time.Second
		if d, err := time.ParseDuration(c.cfg.RateLimit.TokenRefresh); err == nil {
			refresh = d
		}
		tokens := c.cfg.RateLimit.MaxTokens
		if tokens <= 0 {
			tokens = 10000
		}
		c.RateLimiter = security.NewRateLimiter(tokens, refresh)
	}

	// Web 应用防火墙。
	if c.waf.Enabled {
		c.WAF = security.NewWAF(c.waf)
	}

	return nil
}

func (c *SecurityComponent) Start() error {
	tlog.Info(context.Background(), "security component started whitelist=%d blacklist=%d rateLimit=%v waf=%v jwt=%v",
		len(c.cfg.Whitelist),
		len(c.cfg.Blacklist),
		c.cfg.RateLimit.Enabled,
		c.waf.Enabled,
		c.jwt.Enabled)
	return nil
}

func (c *SecurityComponent) Destroy() {
	tlog.Info(context.Background(), "security component destroying")
	if c.RateLimiter != nil {
		c.RateLimiter.Stop()
	}
}
