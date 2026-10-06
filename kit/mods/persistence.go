package mods

import (
	"errors"
	"fmt"
)

const (
	PersistenceDataEngine = "dataengine"
)

var ErrPersistenceEngineSelection = errors.New("persistence: dataengine is the only supported engine")

// PersistenceConfig 是持久化引擎的选择：DataEngine 是唯一的引擎（维护者决定 A4 ① 起是声明）。nest 与 dataengine 的
// Mod 匿名嵌入它，两边共用同一份声明；写别的引擎由声明的枚举拒绝，dataengine.enabled=false 由 ValidateConfig 拒绝。
type PersistenceConfig struct {
	Engine            string `config:"persistence.engine" default:"dataengine" enum:"dataengine" example:"dataengine" help:"持久化引擎；只支持 dataengine"`
	DataEngineEnabled bool   `config:"dataengine.enabled" default:"true" help:"只能是 true：DataEngine 是唯一的持久化引擎，写 false 拒绝启动"`
}

// ValidateConfig 拒绝关掉唯一的持久化引擎。
func (c *PersistenceConfig) ValidateConfig(bool) error {
	if c.Engine != PersistenceDataEngine || !c.DataEngineEnabled {
		return fmt.Errorf("%w: dataengine.enabled=false", ErrPersistenceEngineSelection)
	}
	return nil
}
