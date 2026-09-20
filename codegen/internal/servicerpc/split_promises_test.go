package servicerpc

import (
	"go/ast"
	"slices"
	"strings"
	"testing"
)

// M-10 · ARCH-04：生成的传输拆成两半。传输半（wire 类型、handler 表、BusClient、capability 包装与名字）
// 只 import roost-core，装配半（Server、OwnerCapabilities、ClientMod）才 import roost-kit/mods。
// 拆分前是一个文件，整个传输都拖着 kit 依赖，接口进不了 core 的领域包。
func TestTransportHalfImportsCoreOnlyAndAssemblyHalfOwnsTheMods(t *testing.T) {
	services, err := ParseDir(writeDir(t, goldenService))
	if err != nil {
		t.Fatal(err)
	}
	files, err := Generate(services[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("Generate produced %d files, want 2: %v", len(files), fileNames(files))
	}
	transport, assembly := files[0], files[1]
	if transport.Name != TransportFileName("Shop") || assembly.Name != AssemblyFileName("Shop") {
		t.Fatalf("file names = %v", fileNames(files))
	}
	parsed := parseGenerated(t, services[0])
	imports := func(name string) []string {
		var paths []string
		for _, spec := range parsed[name].Imports {
			paths = append(paths, strings.Trim(spec.Path.Value, "\""))
		}
		return paths
	}
	for _, path := range imports(transport.Name) {
		if strings.HasPrefix(path, "github.com/tjbdwanghaibo/roost-kit") {
			t.Fatalf("the transport half imports %s; an interface that moves into a core domain package would drag kit along", path)
		}
	}
	if !slices.Contains(imports(assembly.Name), "github.com/tjbdwanghaibo/roost-kit/mods") {
		t.Fatal("the assembly half does not import roost-kit/mods, so OwnerCapabilities / ClientMod cannot name the Mods")
	}

	declared := func(name string) map[string]bool {
		out := map[string]bool{}
		for declaredName := range declaredNames(map[string]*ast.File{name: parsed[name]}) {
			out[declaredName] = true
		}
		return out
	}
	inTransport, inAssembly := declared(transport.Name), declared(assembly.Name)
	for _, name := range []string{"ServiceType", "Methods", "RegisterHandlers", "BusClient", "NewBusClient", "Capability", "CapabilityName", "LocalCapabilityName"} {
		if !inTransport[name] {
			t.Fatalf("%s is not declared in the transport half", name)
		}
	}
	for _, name := range []string{"Server", "NewServer", "OwnerCapabilities", "ClientMod", "NewClientMod"} {
		if !inAssembly[name] {
			t.Fatalf("%s is not declared in the assembly half", name)
		}
		if inTransport[name] {
			t.Fatalf("%s is declared in both halves", name)
		}
	}
}
