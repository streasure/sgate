package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mitchellh/mapstructure"
	"gopkg.in/yaml.v3"
)

// loadSettings 读取单个 YAML 文件为 map。
func loadSettings(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	settings := make(map[string]any)
	if err := yaml.Unmarshal(data, &settings); err != nil {
		return nil, err
	}
	return settings, nil
}

// checkPath 校验配置文件路径：必须存在、非目录、扩展名 .yaml/.yml。
func checkPath(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("the path[%s] is a directory", path)
	}

	fileExt := filepath.Ext(path)
	if len(fileExt) == 0 {
		return fmt.Errorf("file[%s] ext not found", path)
	}

	switch strings.ToLower(fileExt) {
	case ".yaml", ".yml":
	default:
		return fmt.Errorf("file[%s] ext %s not supported, only .yaml/.yml can be loaded", path, fileExt)
	}

	return nil
}

// mergeMaps 将 src 递归深合并到 dst，嵌套 map 逐层合并而非整体覆盖。
func mergeMaps(dst, src map[string]any) {
	for k, v := range src {
		srcMap, srcIsMap := v.(map[string]any)
		if !srcIsMap {
			dst[k] = v
			continue
		}
		if dstMap, ok := dst[k].(map[string]any); ok {
			mergeMaps(dstMap, srcMap)
			continue
		}
		merged := make(map[string]any, len(srcMap))
		mergeMaps(merged, srcMap)
		dst[k] = merged
	}
}

// loadFiles 按顺序加载多个 YAML 文件并深合并，映射到结构体（yaml 标签）。
// 原 github.com/streasure/util/uconfig.Load，本地化保留。
func loadFiles[T any](paths ...string) (*T, error) {
	for _, p := range paths {
		if err := checkPath(p); err != nil {
			return nil, err
		}
	}

	merged := make(map[string]any)
	for _, p := range paths {
		settings, err := loadSettings(p)
		if err != nil {
			return nil, err
		}
		mergeMaps(merged, settings)
	}

	cfg := new(T)
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:  cfg,
		TagName: "yaml",
	})
	if err != nil {
		return nil, err
	}
	if err := decoder.Decode(merged); err != nil {
		return nil, err
	}

	return cfg, nil
}
