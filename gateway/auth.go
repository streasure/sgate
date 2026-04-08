package gateway

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims JWT声明
type Claims struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
	jwt.RegisteredClaims
}

// GenerateToken 生成JWT令牌
// 参数:
//
//	userID: 用户ID
//	role: 用户角色
//	secret: 密钥
//
// 返回值:
//
//	string: JWT令牌
//	error: 错误信息
func GenerateToken(userID, role string, secret string) (string, error) {
	claims := &Claims{
		UserID: userID,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// ValidateToken 验证JWT令牌
// 参数:
//
//	tokenString: JWT令牌
//	secret: 密钥
//
// 返回值:
//
//	*Claims: JWT声明
//	error: 错误信息
func ValidateToken(tokenString, secret string) (*Claims, error) {
	claims := &Claims{}

	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		return []byte(secret), nil
	})

	if err != nil {
		return nil, err
	}

	if !token.Valid {
		return nil, errors.New("invalid token")
	}

	return claims, nil
}

// AuthMiddleware 认证中间件
// 参数:
//
//	secret: 密钥
//
// 返回值:
//
//	func(connectionID string, payload map[string]interface{}, callback func(map[string]interface{})): 中间件函数
func AuthMiddleware(secret string) func(connectionID string, payload map[string]interface{}, callback func(map[string]interface{})) {
	return func(connectionID string, payload map[string]interface{}, callback func(map[string]interface{})) {
		// 从payload中获取token
		token, ok := payload["token"].(string)
		if !ok {
			callback(map[string]interface{}{
				"route": "error",
				"data": map[string]interface{}{
					"message": "Missing token",
				},
			})
			return
		}

		// 验证token
		claims, err := ValidateToken(token, secret)
		if err != nil {
			callback(map[string]interface{}{
				"route": "error",
				"data": map[string]interface{}{
					"message": "Invalid token",
				},
			})
			return
		}

		// 将用户信息添加到payload中
		payload["user_id"] = claims.UserID
		payload["role"] = claims.Role

		// 继续处理
		callback(payload)
	}
}