package roost

import (
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// addModuleMod 在只读 Mirror 已装好之后允许模块增加 Remote 写能力。
// remote_entity 已包含 Mirror，不能同时装两个 Mod；实体的 service= 单写者限制保持不变。
func addModuleMod(root, service, name string, warnings io.Writer) error {
	previous, err := LoadManifest(root)
	if err != nil {
		return err
	}
	if name != "remote_entity" || (!contains(previous.SharedMods, "remote_mirror") && !contains(previous.Services[service].Mods, "remote_mirror")) {
		_, err := Add(root, AddOptions{Kind: "mod", Name: name, Service: service, Warnings: warnings})
		return err
	}
	before, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return err
	}
	next := previous
	next.Services = maps.Clone(previous.Services)
	next.SharedMods = slices.DeleteFunc(slices.Clone(previous.SharedMods), func(mod string) bool { return mod == "remote_mirror" })
	for target, spec := range next.Services {
		spec.Mods = slices.Clone(spec.Mods)
		if target == service {
			spec.Mods = slices.DeleteFunc(spec.Mods, func(mod string) bool { return mod == "remote_mirror" })
			spec.Mods = uniqueSorted(append(spec.Mods, "remote_entity"))
		} else if contains(previous.SharedMods, "remote_mirror") && !contains(spec.Mods, "remote_entity") {
			// 从 shared 移走 Mirror 后，保留其他服务原有的能力。
			spec.Mods = uniqueSorted(append(spec.Mods, "remote_mirror"))
		}
		next.Services[target] = spec
	}
	if err := commitManifestSync(root, before, next); err != nil {
		return err
	}
	_, err = appendModConfigSections(root, previous, next, service, warnings)
	return err
}
