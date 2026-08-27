package gateway

import (
	"github.com/streasure/sgate/gateway/cluster"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
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

// AlertOnError 当错误率/延迟突增时触发告警
func (g *Gateway) AlertOnError(title, content string) {
	if g.alertWebhook == nil {
		return
	}
	go g.alertWebhook.Send(g.ctx, cluster.AlertEvent{
		Level:   cluster.AlertError,
		Title:   title,
		Content: content,
		Source:  "sgate-" + g.clusterID,
	})
}
