package cluster

import (
	"context"
	"sync"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/uetcd"
)

type ConfigCenter interface {
	Watch(ctx context.Context) (<-chan []byte, error)
	Pull(ctx context.Context) ([]byte, error)
	Type() string
	Stop()
}

type etcdConfigCenter struct{ component *uetcd.Component }

func NewConfigCenter(cfg config.ConfigCenterConfig, etcdCfg config.EtcdConfig) ConfigCenter {
	if !cfg.Enabled {
		return nil
	}
	key := cfg.DataID
	if key == "" {
		key = "sgate.yaml"
	}
	component := uetcd.New(uetcd.ComponentConfig{
		Etcd:   uetcd.Config{Endpoints: etcdCfg.Endpoints, Endpoint: etcdCfg.Endpoint, Username: etcdCfg.Username, Password: etcdCfg.Password, ServicePrefix: etcdCfg.ServicePrefix, ConfigPrefix: "/config"},
		Config: uetcd.DynamicConfig{Key: key, Format: "yaml"},
	})
	if err := component.Start(); err != nil {
		// 配置中心不可用不阻断网关启动，但必须留下错误日志（否则静默失效难排查）。
		tlog.Error(context.Background(), "config center start failed, dynamic config disabled endpoints=%v error=%v", etcdCfg.Endpoints, err)
		return nil
	}
	return &etcdConfigCenter{component: component}
}

func (c *etcdConfigCenter) Type() string { return "etcd" }
func (c *etcdConfigCenter) Pull(context.Context) ([]byte, error) {
	return c.component.ConfigSnapshot(), nil
}
func (c *etcdConfigCenter) Watch(ctx context.Context) (<-chan []byte, error) {
	ch := make(chan []byte, 4)
	// mu 串行化发送与 close：ctx 取消关通道的同时，迟到的 etcd 回调可能正在
	// select 中命中该通道的 send case（select 不保护 closed channel），无锁必 panic。
	var mu sync.Mutex
	stopped := false
	c.component.OnConfigChange(func(data []byte) {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return
		}
		select {
		case ch <- data:
		default:
		}
	})
	go func() {
		<-ctx.Done()
		mu.Lock()
		defer mu.Unlock()
		if !stopped {
			stopped = true
			close(ch)
		}
	}()
	return ch, nil
}
func (c *etcdConfigCenter) Stop() { c.component.Destroy() }
