#!/usr/bin/env bash
# client_export.config 第 6 节验收（2026-11-17）。用法：acceptance.sh <roost 二进制> <证据输出目录> <工作目录>
# game-demo 工程 + 四张夹具表（两张普通表，其中 monster 带只给服务端的字段与 index；一张竖排键值表；一张 side=c 的表），
# 声明 client_export.config → roost generate，然后逐条核对；C# 在 netstandard2.1 工程里编译并用 net 控制台程序 Load。
set -uo pipefail
roost="$1"; out="$2"; S="$3"
dir="$S/demo"
export GOWORK=off DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1
mkdir -p "$out"
log() { echo "$@" | tee -a "$out/acceptance.txt"; }
step() { log ""; log "### $*"; }
rm -f "$out/acceptance.txt"
rm -rf "$dir" "$S/client" "$S/cs"

step "环境"
log "roost: $("$roost" version)  go: $(go version | cut -d' ' -f3)  dotnet: $(dotnet --version)"

step "1. game-demo 工程 + 夹具表 + client_export.config"
"$roost" project new demo -module example.com/studio/demo -out "$dir" -mods configdata,mongo,nats,dataengine,nest -template game-demo > "$out/1-new.log" 2>&1; log "project new rc=$?"
cd "$dir" || exit 1
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=/Users/whb/roost/roost-core"
git init -q . && git add -A && git -c user.email=a@example.com -c user.name=a commit -qm base
cat > configs/schema/acceptance.go <<'GO'
package schema

// 验收夹具：scene / monster 两张普通表（monster 带只给服务端的 drop_seed、只给客户端的 model、scene_id 上的 index），
// balance 竖排键值表，tip 只给客户端。

//roost:table name=scene key=ID
type Scene struct {
	ID   int32  `csv:"id" json:"id" title:"编号" required:"true"`
	Name string `csv:"name" json:"name" title:"名字" required:"true" unique:"true"`
}

//roost:table name=monster key=ID
type Monster struct {
	ID       int32   `csv:"id" json:"id" title:"编号" required:"true"`
	SceneID  int32   `csv:"scene_id" json:"scene_id" title:"场景" ref:"scene" index:"true"`
	Level    int32   `csv:"level" json:"level" title:"等级" min:"1"`
	DropSeed int64   `csv:"drop_seed" json:"drop_seed" title:"掉落种子" side:"s"`
	Model    string  `csv:"model" json:"model" title:"模型" side:"c" required:"true"`
	Rewards  []int64 `csv:"rewards" json:"rewards" title:"奖励"`
}

//roost:object name=balance layout=kv
type Balance struct {
	MaxLevel   int32  `csv:"max_level" json:"max_level" title:"最高等级" required:"true" min:"1"`
	StartGold  int64  `csv:"start_gold" json:"start_gold" title:"初始金币"`
	ServerTick int32  `csv:"server_tick" json:"server_tick" title:"服务端心跳" side:"s"`
	ShopBanner string `csv:"shop_banner" json:"shop_banner" title:"商店横幅" side:"c"`
}

//roost:table name=tip side=c key=ID
type Tip struct {
	ID   int32  `csv:"id" json:"id" required:"true"`
	Text string `csv:"text" json:"text"`
}
GO
printf 'id,name\n# 场景\n1,forest\n2,cave\n' > configs/table/scene.csv
printf 'id,scene_id,level,drop_seed,model,rewards\n编号,场景,等级,掉落种子,模型,奖励\n# 同一场景可以有多只\n10,1,3,777001,slime.prefab,"[1,2]"\n11,2,5,888002,bat.prefab,\n12,1,4,999003,orc.prefab,[3]\n' > configs/table/monster.csv
printf 'key,value,note\nmax_level,60,玩家等级上限\nstart_gold,100,新角色的金币\nserver_tick,50,只给服务端\n# 只给客户端\nshop_banner,summer_sale,商店顶部横幅\n' > configs/table/balance.csv
printf 'id,text\n1,hello\n2,world\n' > configs/table/tip.csv
cat >> roost.yaml <<'YAML'
client_export:
  protocol: false
  config:
    csharp:
      namespace: Demo.Config
YAML
log "roost.yaml 末尾："; tail -5 roost.yaml | tee -a "$out/acceptance.txt"

step "2. roost generate（protocol: false：不导出协议、不找 protoc，PATH 上没有 protoc 也能跑）"
PATH="$(dirname "$(command -v go)"):/usr/bin:/bin" "$roost" generate > "$out/2-generate.log" 2>&1; log "generate rc=$?"
grep -E "client config|omitted|client-export" "$out/2-generate.log" | tee -a "$out/acceptance.txt"
log "shared/protocol 存在：$(test -e shared/protocol && echo yes || echo no)"
(cd shared/config && find . -type f | sort) | tee -a "$out/acceptance.txt"
cp shared/config/manifest.json "$out/manifest.json"
log "go build ./... && go vet ./configs/...:"; go build ./... > "$out/2-build.log" 2>&1 && go vet ./configs/... >> "$out/2-build.log" 2>&1; log "rc=$?"
"$roost" generate --check > "$out/2-check.log" 2>&1; log "generate --check rc=$? $(tail -1 "$out/2-check.log")"
"$roost" project doctor > "$out/2-doctor.log" 2>&1; log "doctor: $(grep -E 'config-export:fresh' "$out/2-doctor.log")"; log "doctor 里的 client-export:fresh 行数：$(grep -c 'client-export:fresh' "$out/2-doctor.log")"

step "3. 只给服务端的字段不出现在导出目录里"
for word in drop_seed server_tick 掉落种子 服务端心跳 只给服务端; do
  hits=$(grep -rl -- "$word" shared/config | wc -l | tr -d ' '); log "  $word: $hits 个文件"
done
for value in 777001 888002 999003; do
  hits=$(grep -rl -- "$value" shared/config/data | wc -l | tr -d ' '); log "  $value（数据）: $hits 个文件"
done
log "  服务端 configs/data 里只给客户端的值 slime.prefab: $(grep -rl slime.prefab configs/data | wc -l | tr -d ' ') 个文件；tip.json 存在: $(test -f configs/data/tip.json && echo yes || echo no)"
log "  导出的 data/balance.json："; cat shared/config/data/balance.json | tee -a "$out/acceptance.txt"

step "4. 两次生成逐字节相同"
cp -R shared/config "$S/config-1"
"$roost" generate --force > "$out/4-generate-force.log" 2>&1; log "generate --force rc=$?"
diff -r "$S/config-1" shared/config > /dev/null && log "diff -r: 无差异" || log "diff -r: 有差异"
rm -rf "$S/config-1"

step "5. 改 CSV 不重新生成：--check 与 doctor 都报过期"
cp configs/table/monster.csv "$S/monster.csv.bak"
sed -i '' 's/bat.prefab/bat2.prefab/' configs/table/monster.csv
"$roost" generate --check > "$out/5-check-stale.log" 2>&1; log "generate --check rc=$?"; tail -1 "$out/5-check-stale.log" | tee -a "$out/acceptance.txt"
"$roost" project doctor > "$out/5-doctor-stale.log" 2>&1; grep -E 'config-export:fresh' "$out/5-doctor-stale.log" | tee -a "$out/acceptance.txt"
cp "$S/monster.csv.bak" configs/table/monster.csv
sed -i '' 's/777001/777009/' configs/table/monster.csv
"$roost" project doctor > "$out/5-doctor-server-only.log" 2>&1; log "只改只给服务端的列：$(grep -E 'config-export:fresh' "$out/5-doctor-server-only.log")"
cp "$S/monster.csv.bak" configs/table/monster.csv

step "6. C#：netstandard2.1 编译（LangVersion 8、Nullable、警告即错误、XML 文档），net 控制台 Load"
mkdir -p "$S/cs/lib" "$S/cs/app"
cat > "$S/cs/lib/Lib.csproj" <<XML
<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <TargetFramework>netstandard2.1</TargetFramework>
    <LangVersion>8</LangVersion>
    <Nullable>enable</Nullable>
    <TreatWarningsAsErrors>true</TreatWarningsAsErrors>
    <GenerateDocumentationFile>true</GenerateDocumentationFile>
    <EnableDefaultCompileItems>false</EnableDefaultCompileItems>
  </PropertyGroup>
  <ItemGroup>
    <Compile Include="$dir/shared/config/csharp/*.cs" />
  </ItemGroup>
</Project>
XML
major=$(dotnet --version | cut -d. -f1)
cat > "$S/cs/app/App.csproj" <<XML
<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <OutputType>Exe</OutputType>
    <TargetFramework>net$major.0</TargetFramework>
  </PropertyGroup>
  <ItemGroup>
    <ProjectReference Include="../lib/Lib.csproj" />
  </ItemGroup>
</Project>
XML
cat > "$S/cs/app/Program.cs" <<'CS'
using System;
using System.IO;
using System.Linq;
using Demo.Config;

static class Program
{
    static int Main(string[] args)
    {
        Func<string, string> read = name => File.ReadAllText(Path.Combine(args[0], name));
        var tables = ConfigTables.Load(read);
        Console.WriteLine("DataVersion " + ConfigTables.DataVersion + " short " + ConfigTables.ShortDataVersion);
        Console.WriteLine("files " + string.Join(",", ConfigTables.DataFiles()));
        Console.WriteLine("monster " + tables.Monster.Count + " rows; BySceneID(1) = " + string.Join(",", tables.Monster.BySceneID(1).Select(m => m.id + ":" + m.model)) + "; Get(11).rewards = [" + string.Join(",", tables.Monster.Get(11).rewards) + "]");
        Console.WriteLine("scene " + string.Join(",", tables.Scene.All.Select(s => s.id + ":" + s.name)));
        Console.WriteLine("tip " + string.Join(",", tables.Tip.All.Select(t => t.id + ":" + t.text)));
        Console.WriteLine("balance max_level=" + tables.Balance.max_level + " start_gold=" + tables.Balance.start_gold + " shop_banner=" + tables.Balance.shop_banner);
        Console.WriteLine("item " + tables.Item.Count + " rows; building " + tables.Building.Count + " rows");
        var again = ConfigTables.Load(read);
        again.Monster.Get(10).level = 99;
        Console.WriteLine("independent " + (!ReferenceEquals(tables, again) && tables.Monster.Get(10).level == 3));
        try
        {
            ConfigTables.Load(name => name == "monster.json" ? read(name).Replace("slime", "slimy") : read(name));
            Console.WriteLine("UNEXPECTED tampered monster.json loaded");
        }
        catch (ConfigLoadException e)
        {
            Console.WriteLine("tampered: " + e.Message);
        }
        return 0;
    }
}
CS
(cd "$S/cs/app" && dotnet build --disable-build-servers -nologo -v q -o "$S/cs/out" > "$out/6-dotnet-build.log" 2>&1; log "dotnet build rc=$?"; grep -E "警告|错误|Warn|Error" "$out/6-dotnet-build.log" | tail -2 | tee -a "$out/acceptance.txt")
dotnet "$S/cs/out/App.dll" "$dir/shared/config/data" 2>&1 | tee -a "$out/acceptance.txt"
log "manifest data_version: $(grep '"data_version"' shared/config/manifest.json)"
dotnet build-server shutdown > /dev/null 2>&1

step "7. roost client sync：首次"
code="$S/client/Assets/Scripts/Config"; data="$S/client/Assets/StreamingAssets/Config"
"$roost" client sync --from "$dir" --config-code-to "$code" --config-data-to "$data" 2>&1 | tee -a "$out/acceptance.txt"
log "代码目录：$(ls "$code" | tr '\n' ' ')"; log "数据目录：$(ls "$data" | tr '\n' ' ')"
log ".meta 文件：$(find "$S/client" -name '*.meta' | wc -l | tr -d ' ')"

step "8. roost client sync：增量（monster 加一行、去掉 rewards、level 改 int64、加 icon；去掉 tip）"
python3 - configs/schema/acceptance.go <<'PY'
import sys
p = sys.argv[1]; s = open(p).read()
s = s.replace('\tRewards  []int64 `csv:"rewards" json:"rewards" title:"奖励"`\n', '\tIcon     string  `csv:"icon" json:"icon" title:"图标"`\n')
s = s.replace('\tLevel    int32 ', '\tLevel    int64 ')
s = s[:s.index('//roost:table name=tip')]
open(p, 'w').write(s)
PY
rm configs/table/tip.csv
printf 'id,scene_id,level,drop_seed,model,icon\n10,1,3,777001,slime.prefab,s.png\n11,2,5,888002,bat.prefab,\n12,1,4,999003,orc.prefab,\n13,2,6,1,ghost.prefab,\n' > configs/table/monster.csv
"$roost" generate > "$out/8-generate.log" 2>&1; log "generate rc=$?"; grep -E "client config" "$out/8-generate.log" | tee -a "$out/acceptance.txt"
"$roost" client sync --from "$dir" --config-code-to "$code" --config-data-to "$data" 2>&1 | tee -a "$out/acceptance.txt"
log "同步记录 config-manifest.json 的 data_version：$(grep '"data_version"' "$code/config-manifest.json")"

step "9. roost client sync：目录里手写的同名文件拒绝覆盖"
other="$S/client/Other"; mkdir -p "$other/data"
echo '{"mine":true}' > "$other/data/monster.json"
"$roost" client sync --from "$dir" --config-code-to "$other/code" --config-data-to "$other/data" 2>&1 | tee -a "$out/acceptance.txt"; log "rc=${PIPESTATUS[0]}"
log "手写文件：$(cat "$other/data/monster.json")；代码目录被创建：$(test -d "$other/code" && echo yes || echo no)"
log "done"
