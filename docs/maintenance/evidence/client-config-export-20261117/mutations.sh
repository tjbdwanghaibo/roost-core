#!/usr/bin/env bash
# 逐个变异：改一处关键代码、跑对应用例、还原（mutate.py）。C# 相关的用例要 ROOST_TEST_DOTNET=1。
# 用法：mutations.sh <输出文件>
set -u
M="$(dirname "$0")/mutate.py"
R=codegen/internal/roost
T=codegen/internal/tablegen
export ROOST_TEST_DOTNET=1
{
python3 "$M" M1-服务端字段被导出 $T/side.go '"\t\t\tif field.Side == \"\" || field.Side == side {"' '"\t\t\tif true {"' 'TestGenerateClientConfigExport' $R/
python3 "$M" M2-Load不核对哈希 $R/client_config_csharp_runtime.go '"            if (got != wantHash)\n"' '"            if (got != wantHash && wantHash == null)\n"' 'TestClientConfigCSharpRuntime' $R/
python3 "$M" M3a-Load不查required/unique/min/enum $R/client_config_csharp_runtime.go '"            foreach (var rule in rules)\n"' '"            foreach (var rule in new ConfigRule[0])\n"' 'TestClientConfigCSharpRuntime' $R/
python3 "$M" M3b-Load不查ref $R/client_config_csharp.go '"\t\tfmt.Fprintf(&b, \"            t%d.CheckRefs(%s);\\n\", i, strings.Join(args, \", \"))"' '"\t\t_, _ = args, i"' 'TestClientConfigCSharpRuntime' $R/
python3 "$M" M4-sync覆盖不是上次写的文件 $R/client_sync.go '"\t\t\tif !ours {"' '"\t\t\tif !ours && previous.label != \"\" {"' 'TestClientConfigSync|TestClientSync' $R/
python3 "$M" M5a-check不认领配置导出目录 $R/client_export.go '"\tif spec.Config.owns(rel) {\n\t\treturn true\n\t}\n"' '""' 'TestGenerateClientConfigExport' $R/
python3 "$M" M5b-doctor不比数据哈希 $R/client_config_export.go '"\t\tcase old.DataHash != table.DataHash:"' '"\t\tcase false:"' 'TestGenerateClientConfigExport' $R/
python3 "$M" M5c-doctor不比schema哈希 $R/client_config_export.go '"\t\tcase old.SchemaHash != table.SchemaHash:"' '"\t\tcase false:"' 'TestGenerateClientConfigExport' $R/
python3 "$M" M6-DataVersion不含表名 $T/export.go '"\"%s\\x00%s\\n\", table.Name, table.DataHash"' '"\"%s%s\", \"\", table.DataHash"' 'TestClientExportDataVersion' $T/
python3 "$M" M7-横排Convert不跳过注释 $T/main.go '"\treader.TrimLeadingSpace = true\n\treader.Comment = \x27#\x27\n\trecords, err := reader.ReadAll()"' '"\treader.TrimLeadingSpace = true\n\trecords, err := reader.ReadAll()"' 'TestClientExportFixtureEndToEnd' $T/
python3 "$M" M8a-没声明config也跑配置导出 $R/generate.go '"\tif m.ClientExport != nil && m.ClientExport.Config != nil {\n\t\tgenerators = append"' '"\tif m.ClientExport != nil {\n\t\tgenerators = append"' 'TestUndeclaredClientConfigExportChangesNothing' $R/
python3 "$M" M8b-只写config也导出协议 $R/client_export.go '"\treturn spec.Config == nil || strings.TrimSpace(spec.Dir) != \"\""' '"\treturn true || strings.TrimSpace(spec.Dir) != \"\""' 'TestUndeclaredClientConfigExportChangesNothing|TestClientConfigExportSpecValidation' $R/
python3 "$M" M9-sync不删上次写过这次没有的文件 $R/client_sync.go '"\t\tif _, err := os.Stat(target); err == nil {\n\t\t\tplan.removed"' '"\t\tif _, err := os.Stat(target); err != nil {\n\t\t\tplan.removed"' 'TestClientConfigSync' $R/
python3 "$M" M10-sync报告不比字段类型 $R/client_sync.go '"\t\t\tcase describeField(before) != describeField(field):"' '"\t\t\tcase false:"' 'TestClientConfigSync' $R/
python3 "$M" M11-生成不删退役文件 $R/client_config_export.go '"\t\t\tif _, keep := export.files[name]; keep || "' '"\t\t\tif _, keep := export.files[name]; true || keep || "' 'TestClientConfigExportTransactionalRetirement' $R/
python3 "$M" M12-tables不过滤 $R/client_config_export.go '"\tif len(spec.Tables) == 0 {\n\t\treturn model.Tables, 0, nil"' '"\tif true {\n\t\treturn model.Tables, 0, nil"' 'TestClientConfigExportTables' $R/
python3 "$M" M13-config目录可放在configs下 $R/client_export.go '"\t\tcase \"protocol\", \"db\", \"game\", \"internal\", \"service\", \"configs\", \"cmd\", \"deploy\":"' '"\t\tcase \"protocol\", \"db\", \"internal\", \"service\", \"cmd\", \"deploy\":"' 'TestClientConfigExportSpecValidation' $R/
} 2>&1 | tee "$1"
