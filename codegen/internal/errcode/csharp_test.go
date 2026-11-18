package errcode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateCSharp(t *testing.T) {
	defs := []Definition{
		{Code: 100000, Name: "shop.item_not_found", Message: "商店商品不存在"},
		{Code: 100001, Name: "9lives", Message: "a \"quoted\" \\ <tag> & line\nbreak"},
	}
	raw, err := GenerateCSharp(defs, "Game.Protocol")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		CSharpGeneratedHeader + "\n",
		"namespace Game.Protocol\n",
		"/// <summary>商店商品不存在</summary>\n        public const int ShopItemNotFound = 100000;",
		"/// <summary>a \"quoted\" \\ &lt;tag&gt; &amp; line break</summary>\n        public const int _9lives = 100001;",
		`case ShopItemNotFound: return "shop.item_not_found";`,
		`case _9lives: return "a \"quoted\" \\ <tag> & line\nbreak";`,
		"default: return null;",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("ErrorCodes.cs missing %q:\n%s", want, text)
		}
	}
	if _, err := GenerateCSharp([]Definition{{Code: 1, Name: "a_b"}, {Code: 2, Name: "a.b"}}, "N"); err == nil || !strings.Contains(err.Error(), "same") && !strings.Contains(err.Error(), "both become") {
		t.Fatalf("identifier collision accepted: %v", err)
	}
	if _, err := GenerateCSharp([]Definition{{Code: 1, Name: "中文"}}, "N"); err == nil {
		t.Fatal("name without ASCII letters accepted")
	}
}

// csharpClashingNames 是会撞上 ErrorCodes 自身符号的合法 errcode 名：生成的类名、Name / Message 两个方法，
// 以及静态类从 object 继承来的成员（同名会隐藏继承成员，警告即错误时编译失败）。profile_name 是普通名字的对照。
var csharpClashingNames = []string{
	"profile_name", "name", "message", "error_codes",
	"equals", "get_hash_code", "to_string", "get_type", "reference_equals", "memberwise_clone",
}

// 撞名的常量改名（加后缀 _），Name / Message 按码仍答原来的 errcode 名与说明；普通名字不受影响（RV-20261117-02）。
func TestGenerateCSharpRenamesIdentifiersThatClashWithItsOwnSymbols(t *testing.T) {
	defs := make([]Definition, len(csharpClashingNames))
	for i, name := range csharpClashingNames {
		defs[i] = Definition{Code: 190001 + int32(i), Name: name, Message: "review " + name}
	}
	raw, err := GenerateCSharp(defs, "Review.Protocol")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		"public const int ProfileName = 190001;",
		"public const int Name_ = 190002;",
		"public const int Message_ = 190003;",
		"public const int ErrorCodes_ = 190004;",
		"public const int Equals_ = 190005;",
		"public const int MemberwiseClone_ = 190010;",
		`case Name_: return "name";`,
		`case Message_: return "review message";`,
		"public static string Name(int code)",
		"public static string Message(int code)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("ErrorCodes.cs missing %q:\n%s", want, text)
		}
	}
	// 后缀不会和别的名字撞：别的名字转出来的标识符只有字母数字，不以 _ 结尾；转成同一个标识符的照旧报错。
	if _, err := GenerateCSharp([]Definition{{Code: 1, Name: "name"}, {Code: 2, Name: "name_"}}, "N"); err == nil || !strings.Contains(err.Error(), "both become") {
		t.Fatalf("name and name_ (both Name) accepted: %v", err)
	}
}

// TestGenerateCSharpCompiles 用 .NET SDK 实际编译 ErrorCodes.cs（netstandard2.1、LangVersion 8、Nullable、警告即错误、
// XML 文档，与 client export 配置表 C# 的编译严格度相同），覆盖撞名的合法 errcode 名和普通名字（RV-20261117-02）。
// 本机有 dotnet 就随 go test 运行（不下载依赖），没有就 SKIP；ROOST_REQUIRE_DOTNET=1 时缺 dotnet 直接失败。
func TestGenerateCSharpCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the generated C# with dotnet")
	}
	dotnet, err := exec.LookPath("dotnet")
	if err != nil {
		if os.Getenv("ROOST_REQUIRE_DOTNET") == "1" {
			t.Fatalf("ROOST_REQUIRE_DOTNET=1 but dotnet is not on PATH: %v", err)
		}
		t.Skip(".NET SDK unavailable; Go tests do not prove ErrorCodes.cs compiles (set ROOST_REQUIRE_DOTNET=1 to fail instead)")
	}
	// 走正式扫描：errcode.Define 写进 Go 源文件，经 Definitions 读出再导出。
	var source strings.Builder
	source.WriteString("package errors\n\nimport \"github.com/tjbdwanghaibo/roost-core/infra/base/errcode\"\n\nvar (\n")
	for i, name := range csharpClashingNames {
		fmt.Fprintf(&source, "\tErr%d = errcode.Define(%d, %q, %q)\n", i, 190001+i, name, "review "+name)
	}
	source.WriteString(")\n")
	work := t.TempDir()
	writeTree(t, work, map[string]string{"errors/errors.go": source.String()})
	defs, err := Definitions(filepath.Join(work, "errors"))
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != len(csharpClashingNames) {
		t.Fatalf("definitions = %+v, want %d", defs, len(csharpClashingNames))
	}
	raw, err := GenerateCSharp(defs, "Review.Protocol")
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, work, map[string]string{
		"lib/ErrorCodes.cs": string(raw),
		"lib/Lib.csproj": `<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <TargetFramework>netstandard2.1</TargetFramework>
    <LangVersion>8</LangVersion>
    <Nullable>enable</Nullable>
    <TreatWarningsAsErrors>true</TreatWarningsAsErrors>
    <GenerateDocumentationFile>true</GenerateDocumentationFile>
  </PropertyGroup>
</Project>
`,
	})
	env := append(os.Environ(), "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1", "DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, dotnet, "build", "--disable-build-servers", "-nologo", "-v", "q", "-o", filepath.Join(work, "out"), filepath.Join(work, "lib", "Lib.csproj"))
	build.Env = env
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("dotnet build ErrorCodes.cs: %v\n%s\n--- ErrorCodes.cs ---\n%s", err, output, raw)
	}
}
