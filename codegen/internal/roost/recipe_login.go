package roost

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// writeRecipeLoginDevConfig 只在新工程的暂存树中执行。配方提供可直接试用的开发入口，
// 不修改生产配置、Secret 示例或业务渠道；正式部署仍由 login 模块拒绝开发令牌。
// 地址取已生成的业务服务配置，不假定服务叫 game 或硬编码它的 SID/端口。
func writeRecipeLoginDevConfig(root, service string) error {
	path := filepath.Join(root, "configs/service/config."+service+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var game struct {
		SID          int32 `yaml:"sid"`
		PlayerAccess struct {
			TCP struct {
				Addr string `yaml:"addr"`
			} `yaml:"tcp"`
		} `yaml:"player_access"`
	}
	if err := yaml.Unmarshal(raw, &game); err != nil {
		return fmt.Errorf("recipe login: %s: %w", path, err)
	}
	_, port, err := net.SplitHostPort(game.PlayerAccess.TCP.Addr)
	if err != nil || game.SID <= 0 {
		return fmt.Errorf("recipe login: %s needs a positive sid and player_access.tcp.addr as host:port", path)
	}
	path = filepath.Join(root, "configs/service/config.account.yaml")
	raw, err = os.ReadFile(path)
	if err != nil {
		return err
	}
	body, changed := appendMissingConfigBlocks(string(raw), []string{
		"# Development only. The login module rejects this token in production.\nlogin:\n  dev_token: dev-recipe-local\n",
		fmt.Sprintf("# Local client endpoint. Deployments must supply their public Gate addresses.\nportal:\n  servers:\n    \"%d\": 127.0.0.1:%s\n", game.SID, port),
	}, false)
	if !changed {
		return nil
	}
	return writeAtomic(path, []byte(body), 0o644)
}
