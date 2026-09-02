package gateway

import (
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/tlog"
	"gopkg.in/yaml.v3"
)

// startConfigCenterWatcher 启动配置中心监听并桥接到现有 handleConfigUpdate
func (g *Gateway) startConfigCenterWatcher() {
	if g.configCenter == nil {
		return
	}
	ch, err := g.configCenter.Watch(g.ctx)
	if err != nil {
		tlog.Error("config center watch failed", "error", err)
		return
	}
	go func() {
		for yamlBytes := range ch {
			if len(yamlBytes) == 0 {
				continue
			}
			currentCfg := g.cfg.Load().(*config.Config)
			newCfg := *currentCfg
			if err := yaml.Unmarshal(yamlBytes, &newCfg); err != nil {
				tlog.Warn("config center content parse failed", "error", err)
				continue
			}
			g.configUpdateChan <- &newCfg
			tlog.Info("config updated from config center",
				"type", g.configCenter.Type())
		}
	}()
}
