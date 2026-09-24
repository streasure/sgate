package internal

import (
	"context"
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
		tlog.Error(context.TODO(), "config center watch failed error=%v", err)
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
				tlog.Warn(context.TODO(), "config center content parse failed error=%v", err)
				continue
			}
			select {
			case g.configUpdateChan <- &newCfg:
			case <-g.stopChan:
				return
			}
			tlog.Info(context.TODO(), "config updated from config center type=%s",
				g.configCenter.Type())
		}
	}()
}
