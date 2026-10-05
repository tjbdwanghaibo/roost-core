// Command bundle 是 hotcode 真实插件测试用的最小补丁包，由 hotcode/plugintest 的测试用
// `go build -buildmode=plugin` 现场构建（testdata 不参与 go build ./...）。
//
// Apply 先替换 plugintest.first，再替换 plugintest.second；测试把 second 注册成不同签名，
// 第二步就会以 ErrTypeMismatch 失败，形成“部分应用”。second 没注册时跳过。
package main

import (
	"errors"

	"github.com/tjbdwanghaibo/roost-core/hotcode"
)

type bundle struct{}

func (bundle) Meta() hotcode.Meta {
	return hotcode.Meta{Version: "plugintest-1", Source: "hotcode/plugintest/testdata/bundle"}
}

func (b bundle) Apply(r *hotcode.Registry) error {
	if err := r.Replace("plugintest.first", func(v int) int { return v + 100 }, b.Meta()); err != nil {
		return err
	}
	if err := r.Replace("plugintest.second", func(v int) int { return v + 200 }, b.Meta()); err != nil && !errors.Is(err, hotcode.ErrNotFound) {
		return err
	}
	return nil
}

func (bundle) Revert(r *hotcode.Registry) error {
	err := r.Revert("plugintest.first")
	if secondErr := r.Revert("plugintest.second"); secondErr != nil && !errors.Is(secondErr, hotcode.ErrNotFound) {
		err = errors.Join(err, secondErr)
	}
	return err
}

// PatchBundle 以接口变量导出：Lookup 得到 *hotcode.Bundle，走 LoadPlugin 的指针分支。
var PatchBundle hotcode.Bundle = bundle{}

func main() {}
