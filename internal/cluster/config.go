package cluster

import (
	"context"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/etcd"
)

type ConfigCenter interface {
	Watch(ctx context.Context) (<-chan []byte, error)
	Pull(ctx context.Context) ([]byte, error)
	Type() string
	Stop()
}

type etcdConfigCenter struct{ component *etcd.Component }

func NewConfigCenter(cfg config.ConfigCenterConfig, etcdCfg config.EtcdConfig) ConfigCenter {
	if !cfg.Enabled || !etcdCfg.Enabled {
		return nil
	}
	key := cfg.DataID
	if key == "" {
		key = "sgate.yaml"
	}
	component := etcd.New(etcd.ComponentConfig{
		Enabled: true,
		Etcd:    etcd.Config{Endpoints: etcdCfg.Endpoints, Endpoint: etcdCfg.Endpoint, Username: etcdCfg.Username, Password: etcdCfg.Password, ServicePrefix: etcdCfg.ServicePrefix, ConfigPrefix: "/config"},
		Config:  etcd.DynamicConfig{Enabled: true, Key: key, Format: "yaml"},
	})
	if err := component.Start(); err != nil {
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
	c.component.OnConfigChange(func(data []byte) {
		select {
		case ch <- data:
		default:
		}
	})
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}
func (c *etcdConfigCenter) Stop() { c.component.Destroy() }
