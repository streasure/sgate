package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	json "github.com/bytedance/sonic"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/gatewayutil"
	"github.com/streasure/sgate/internal/types"
)

// JWTAuthFilter JWT 鉴权过滤器
// 实现 RFC 7519 HS256，无第三方依赖
type JWTAuthFilter struct {
	secret     []byte
	issuer     string
	skipRoutes map[string]struct{} // 跳过鉴权的路由（如 handshake/login）
	headerName string              // JWT 所在字段名（默认 X-Auth-Token）
	mu         sync.RWMutex
	revoked    map[string]int64 // jti -> 过期时间（黑名单）
}

// JWTClaims JWT 载荷
type JWTClaims struct {
	Sub   string `json:"sub"` // 用户 ID
	Iss   string `json:"iss"` // 签发方
	Exp   int64  `json:"exp"` // 过期时间
	Iat   int64  `json:"iat"` // 签发时间
	Jti   string `json:"jti"` // 唯一 ID（用于撤销）
	Route string `json:"route,omitempty"`
}

// NewJWTAuthFilter 构造 JWT 鉴权过滤器
func NewJWTAuthFilter(cfg config.JWTAuthConfig) *JWTAuthFilter {
	f := &JWTAuthFilter{
		secret:     []byte(cfg.Secret),
		issuer:     cfg.Issuer,
		headerName: cfg.HeaderField,
		skipRoutes: make(map[string]struct{}),
		revoked:    make(map[string]int64),
	}
	if f.headerName == "" {
		f.headerName = "X-Auth-Token"
	}
	for _, r := range cfg.SkipRoutes {
		f.skipRoutes[r] = struct{}{}
	}
	return f
}

func (f *JWTAuthFilter) Name() string             { return "jwt-auth" }
func (f *JWTAuthFilter) Phase() types.FilterPhase { return types.PhaseAuth }
func (f *JWTAuthFilter) Priority() int            { return 100 }

// Process 鉴权处理：从元数据取 token，校验签名+过期+撤销。
// fail-closed：连接已绑定用户（UserUUID 非空）且无 token → 拒绝；
// 未绑定（LoginGate 前/未登录帧）→ 放行，由 loginKey/流水线兜底。
// skipRoutes 支持路由名与 cmd 十进制串两种键。
func (f *JWTAuthFilter) Process(fc *types.FilterContext) (bool, error) {
	if len(f.skipRoutes) > 0 {
		if _, ok := f.skipRoutes[fc.Route]; ok {
			return true, nil
		}
	}
	token := fc.Metadata[f.headerName]
	if token == "" {
		if fc.UserUUID == "" {
			// 未绑定用户：不在此处强制 JWT（LoginGate/preAuth 负责建立会话）
			return true, nil
		}
		fc.DropReason = "missing jwt token"
		return false, nil
	}
	claims, err := f.Validate(token)
	if err != nil {
		fc.DropReason = "invalid jwt: " + err.Error()
		return false, nil
	}
	fc.UserUUID = claims.Sub
	fc.Metadata["jwt.jti"] = claims.Jti
	fc.Metadata["jwt.exp"] = strconv.FormatInt(claims.Exp, 10)
	return true, nil
}

// Validate 校验 JWT
func (f *JWTAuthFilter) Validate(token string) (*JWTClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid token format")
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var h struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(header, &h); err != nil {
		return nil, err
	}
	if h.Alg != "HS256" {
		return nil, errors.New("unsupported alg: " + h.Alg)
	}
	// 验签
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	validSignature := hmac.Equal(sig, f.sign(signingInput))
	if !validSignature {
		return nil, errors.New("signature mismatch")
	}
	// 解析 claims
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims JWTClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	// 过期校验
	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return nil, errors.New("token expired")
	}
	// 签发方校验
	if f.issuer != "" && claims.Iss != f.issuer {
		return nil, errors.New("issuer mismatch")
	}
	// 撤销校验
	f.mu.RLock()
	exp, ok := f.revoked[claims.Jti]
	f.mu.RUnlock()
	if ok && time.Now().Unix() < exp {
		return nil, errors.New("token revoked")
	}
	return &claims, nil
}

// Revoke 撤销 token（按 jti）。写入时顺带清理已过期的撤销记录，防止 map 无限增长。
func (f *JWTAuthFilter) Revoke(jti string, exp int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().Unix()
	if len(f.revoked) > 1024 {
		for k, e := range f.revoked {
			if e <= now {
				delete(f.revoked, k)
			}
		}
	}
	f.revoked[jti] = exp
}

// UpdateSecret 动态更新密钥
func (f *JWTAuthFilter) UpdateSecret(secret string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secret = []byte(secret)
}

func (f *JWTAuthFilter) sign(input string) []byte {
	f.mu.RLock()
	defer f.mu.RUnlock()
	mac := hmac.New(sha256.New, f.secret)
	mac.Write([]byte(input))
	return mac.Sum(nil)
}

// Issue 仅供测试或本地签发使用
func (f *JWTAuthFilter) Issue(claims JWTClaims) (string, error) {
	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	h := base64.RawURLEncoding.EncodeToString(hb)
	p := base64.RawURLEncoding.EncodeToString(pb)
	sig := base64.RawURLEncoding.EncodeToString(f.sign(h + "." + p))
	return h + "." + p + "." + sig, nil
}

// init 自动注册到 SPI 注册表
func init() {
	types.RegisterFilter("jwt-auth", func(cfg map[string]any) (types.Filter, error) {
		c := config.JWTAuthConfig{
			Secret:      gatewayutil.GetString(cfg, "secret"),
			Issuer:      gatewayutil.GetString(cfg, "issuer"),
			HeaderField: gatewayutil.GetString(cfg, "headerField"),
		}
		// 跳过配置中指定的路由。
		if v, ok := cfg["skipRoutes"]; ok {
			if arr, ok := v.([]any); ok {
				for _, x := range arr {
					if route, ok := x.(string); ok {
						c.SkipRoutes = append(c.SkipRoutes, route)
					}
				}
			}
		}
		return NewJWTAuthFilter(c), nil
	})
}
