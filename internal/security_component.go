package gateway

import (
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/security"
	"github.com/streasure/sgate/types"
	"github.com/streasure/util/component"
	"github.com/streasure/util/tlog"
)

// SecurityComponent manages the lifecycle of all security sub-modules:
// WhitelistBlacklist, WAF, RateLimiter, JWTAuthFilter, CircuitBreakerManager.
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
	tlog.Info("security component init")

	c.WhitelistBlacklist = security.NewWhitelistBlacklist()
	c.CircuitBreakerMgr = security.NewCircuitBreakerManager()

	// Whitelist / Blacklist
	if c.cfg.Enabled {
		for _, ip := range c.cfg.Whitelist {
			c.WhitelistBlacklist.AddToWhitelist(ip)
		}
		for _, ip := range c.cfg.Blacklist {
			c.WhitelistBlacklist.AddToBlacklist(ip)
		}
	}

	// JWT
	if c.jwt.Enabled {
		c.JWTAuth = security.NewJWTAuthFilter(c.jwt)
		c.FilterChain.AddFilter(c.JWTAuth)
	}

	// Rate Limiter
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

	// WAF
	if c.waf.Enabled {
		c.WAF = security.NewWAF(c.waf)
	}

	return nil
}

func (c *SecurityComponent) Start() error {
	tlog.Info("security component started",
		"whitelist", len(c.cfg.Whitelist),
		"blacklist", len(c.cfg.Blacklist),
		"rateLimit", c.cfg.RateLimit.Enabled,
		"waf", c.waf.Enabled,
		"jwt", c.jwt.Enabled)
	return nil
}

func (c *SecurityComponent) Destroy() {
	tlog.Info("security component destroying")
	if c.RateLimiter != nil {
		c.RateLimiter.Stop()
	}
}
