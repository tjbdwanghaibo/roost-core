//go:build darwin || linux || freebsd

package hotcode

import (
	"fmt"
	"path/filepath"
	"plugin"
)

// Bundle is implemented by hot-code plugin packages.
//
// A plugin should export a symbol named PatchBundle whose dynamic type
// implements this interface.
type Bundle interface {
	Meta() Meta
	Apply(*Registry) error
	Revert(*Registry) error
}

// LoadPlugin opens a Go plugin and applies its PatchBundle. The Go runtime does
// not unload plugins, so rollback must be implemented by the bundle's Revert
// method or by reverting individual patch points.
//
// Apply 失败（错误或 panic）时，已被它替换的补丁点恢复成加载前的那一代，再返回错误
// （Registry.ApplyBundle，RR-20261005-NC-245）。
func LoadPlugin(path string) (Bundle, error) {
	if path == "" {
		return nil, fmt.Errorf("hotcode: plugin path required")
	}
	if filepath.Ext(path) != ".so" {
		return nil, fmt.Errorf("hotcode: plugin must be a .so file: %s", path)
	}
	p, err := plugin.Open(path)
	if err != nil {
		return nil, fmt.Errorf("hotcode: open plugin %s: %w", path, err)
	}
	sym, err := p.Lookup("PatchBundle")
	if err != nil {
		return nil, fmt.Errorf("hotcode: lookup PatchBundle in %s: %w", path, err)
	}
	bundle, ok := sym.(Bundle)
	if !ok {
		// 导出成 hotcode.Bundle 接口变量时 Lookup 得到 *Bundle。之前这里写成
		// `if ptr, ok := ...; ok && ptr != nil { ok = bundle != nil }`，内层 ok 遮住了外层，
		// 这种导出方式一律被拒（RR-20261005-NC-244）。
		if ptr, isPtr := sym.(*Bundle); isPtr && ptr != nil {
			bundle = *ptr
			ok = bundle != nil
		}
	}
	if !ok {
		return nil, fmt.Errorf("hotcode: PatchBundle in %s does not implement hotcode.Bundle", path)
	}
	if restored, err := Default.ApplyBundle(bundle); err != nil {
		return nil, fmt.Errorf("hotcode: apply plugin %s (rolled back %d patch point(s)): %w", path, restored, err)
	}
	return bundle, nil
}
