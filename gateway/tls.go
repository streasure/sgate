package gateway

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"

	tlog "github.com/streasure/treasure-slog"
)

type TLSConfig struct {
	Enabled      bool   // 是否启用 TLS
	CertFile     string // TLS 证书文件路径
	KeyFile      string // TLS 私钥文件路径
	ClientAuth   bool   // 是否要求客户端证书
	MinVersion   uint16 // 最小 TLS 版本
	PreferServer bool   // 是否优先使用服务器加密套件
}

type TLSManager struct {
	config TLSConfig
	cert   tls.Certificate
}

var tlsManager *TLSManager

func NewTLSManager(config TLSConfig) (*TLSManager, error) {
	if !config.Enabled {
		tlog.Info("TLS 已被禁用")
		return nil, nil
	}

	tlsManager = &TLSManager{
		config: config,
	}

	// 加载证书
	var err error
	if config.CertFile != "" && config.KeyFile != "" {
		tlsManager.cert, err = tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
		if err != nil {
			tlog.Error("加载 TLS 证书失败", "error", err, "certFile", config.CertFile, "keyFile", config.KeyFile)
			return nil, err
		}
		tlog.Info("TLS 证书加载成功", "certFile", config.CertFile)
	} else {
		// 生成自签名证书（仅用于测试）
		tlog.Warn("未提供 TLS 证书，生成自签名证书（仅用于测试）")
		tlsManager.cert, err = generateSelfSignedCert()
		if err != nil {
			tlog.Error("生成自签名证书失败", "error", err)
			return nil, err
		}
	}

	return tlsManager, nil
}

func GetTLSManager() *TLSManager {
	return tlsManager
}

func (tm *TLSManager) GetTLSConfig() *tls.Config {
	if tm == nil {
		return nil
	}

	config := &tls.Config{
		Certificates:             []tls.Certificate{tm.cert},
		ClientAuth:               tls.NoClientCert,
		MinVersion:               tm.config.MinVersion,
		PreferServerCipherSuites: tm.config.PreferServer,
	}

	if tm.config.ClientAuth {
		config.ClientAuth = tls.RequireAnyClientCert
	}

	return config
}

func (tm *TLSManager) IsEnabled() bool {
	return tm != nil && tm.config.Enabled
}

func (tm *TLSManager) GetCert() tls.Certificate {
	if tm == nil {
		return tls.Certificate{}
	}
	return tm.cert
}

func generateSelfSignedCert() (tls.Certificate, error) {
	// 生成 RSA 私钥
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("生成 RSA 密钥失败: %w", err)
	}

	// 创建证书模板
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("生成序列号失败: %w", err)
	}

	certTemplate := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"SGate Gateway"},
			CommonName:  "localhost",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}

	// 创建证书
	certDER, err := x509.CreateCertificate(rand.Reader, &certTemplate, &certTemplate, &privateKey.PublicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("创建证书失败: %w", err)
	}

	// 编码私钥
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})

	// 编码证书
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	// 解析证书和私钥
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("解析证书失败: %w", err)
	}

	return cert, nil
}

func SaveSelfSignedCert(certFile, keyFile string) error {
	if tlsManager == nil {
		return fmt.Errorf("TLS 管理器未初始化")
	}

	cert := tlsManager.cert

	// 保存证书
	certOut, err := os.Create(certFile)
	if err != nil {
		return fmt.Errorf("创建证书文件失败: %w", err)
	}
	defer certOut.Close()

	for _, cert := range cert.Certificate {
		pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: cert})
	}
	tlog.Info("TLS 证书已保存", "certFile", certFile)

	// 注意：私钥无法从 tls.Certificate 中提取，这里只保存了证书
	// 在生产环境中，应该使用外部生成的证书

	return nil
}

func DefaultTLSConfig() TLSConfig {
	return TLSConfig{
		Enabled:      false, // 默认禁用 TLS
		CertFile:     "",
		KeyFile:      "",
		ClientAuth:   false,
		MinVersion:   tls.VersionTLS12, // TLS 1.2
		PreferServer: true,
	}
}

func ProductionTLSConfig() TLSConfig {
	return TLSConfig{
		Enabled:      true, // 生产环境启用 TLS
		CertFile:     "/etc/sgate/tls/cert.pem",
		KeyFile:      "/etc/sgate/tls/key.pem",
		ClientAuth:   false,
		MinVersion:   tls.VersionTLS12, // TLS 1.2
		PreferServer: true,
	}
}
