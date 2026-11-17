package tablegen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseMetaRootUsesTargetProjectModulePath(t *testing.T) {
	root := t.TempDir()
	writeTablegenTestFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n")
	metaDir := filepath.Join(root, "configs", "schema", "game")
	writeTablegenTestFile(t, filepath.Join(metaDir, "monster.go"), `package game

//roost:table name=monster file=monster.csv json=monster.json key=ID
type Monster struct {
	ID int32 `+"`csv:\"id\" json:\"id\"`"+`
}
`)

	metas, err := parseMetaRoot(filepath.Join(root, "configs", "schema"))
	if err != nil {
		t.Fatalf("parse meta root: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("meta count = %d, want 1", len(metas))
	}
	if got, want := metas[0].ImportPath, "example.com/game/configs/schema/game"; got != want {
		t.Fatalf("import path = %q, want %q", got, want)
	}
}

func writeTablegenTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestUnlabelledFieldsRoundTripThroughGeneratedJSON(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "schema.go")
	writeTablegenTestFile(t, path, "package schema\n//roost:table key=MaxStack\ntype Item struct { MaxStack int; Level int; HP int }\n")
	metas, err := parseMetaFile(root, "example.com/test", path)
	if err != nil {
		t.Fatal(err)
	}
	csvDir := filepath.Join(root, "csv")
	jsonDir := filepath.Join(root, "json")
	writeTablegenTestFile(t, filepath.Join(csvDir, "item.csv"), "max_stack,level,h_p\n42,7,9\n")
	if err := convertCSVToJSON(metas, csvDir, jsonDir, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(jsonDir, "item.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		MaxStack int
		Level    int
		HP       int
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].MaxStack != 42 || got[0].Level != 7 || got[0].HP != 9 {
		t.Fatalf("JSON silently loses unlabelled fields: %s => %+v", raw, got)
	}
}
func TestUnknownTableKeyIsRejected(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "schema.go")
	writeTablegenTestFile(t, path, "package schema\n//roost:table key=Typo\ntype Item struct { ID int }\n")
	if _, err := parseMetaFile(root, "example.com/test", path); err == nil {
		t.Fatal("unknown key silently selected first field")
	}
}
func TestObjectCSVRejectsMultipleDataRows(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "schema.go")
	writeTablegenTestFile(t, path, "package schema\n//roost:object\ntype Setting struct { Limit int }\n")
	metas, err := parseMetaFile(root, "example.com/test", path)
	if err != nil {
		t.Fatal(err)
	}
	writeTablegenTestFile(t, filepath.Join(root, "csv", "setting.csv"), "limit\n1\n2\n")
	if err := convertCSVToJSON(metas, filepath.Join(root, "csv"), filepath.Join(root, "json"), false, io.Discard); err == nil {
		t.Fatal("second object row silently discarded")
	}
}

func TestGeneratedLoaderAndObjectConverterMatchSchema(t *testing.T) {
	root := t.TempDir()
	core, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	writeTablegenTestFile(t, filepath.Join(root, "go.mod"), fmt.Sprintf("module example.com/tablecheck\ngo 1.27.0\nrequire github.com/tjbdwanghaibo/roost-core v0.0.0\nreplace github.com/tjbdwanghaibo/roost-core => %s\n", core))
	writeTablegenTestFile(t, filepath.Join(root, "schema", "item.go"), "package schema\n//roost:table key=MaxStack\ntype Item struct { MaxStack int; Level int; HP int }\n//roost:object\ntype Setting struct { Limit int }\n")
	metas, err := parseMetaRoot(filepath.Join(root, "schema"))
	if err != nil {
		t.Fatal(err)
	}
	writeTablegenTestFile(t, filepath.Join(root, "csv", "item.csv"), "max_stack,level,h_p\n42,7,9\n")
	writeTablegenTestFile(t, filepath.Join(root, "csv", "setting.csv"), "limit\n5\n")
	if err := convertCSVToJSON(metas, filepath.Join(root, "csv"), filepath.Join(root, "data"), false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := generateGo(metas, filepath.Join(root, "generated"), "generated", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	writeTablegenTestFile(t, filepath.Join(root, "main.go"), `package main
import("context";"strings";"example.com/tablecheck/generated";"github.com/tjbdwanghaibo/roost-core/framework/configdata")
func main(){
 r:=configdata.NewRegistry();if err:=generated.RegisterGeneratedConfigData(r);err!=nil{panic(err)}
 snap,err:=configdata.NewStore(r,"data").Load(context.Background());if err!=nil{panic(err)}
 table,ok:=generated.ItemTableFrom(snap);if !ok{panic("missing table")}
 row,ok:=table.Get(42);if !ok||row.Level!=7||row.HP!=9{panic("fields lost in generated runtime loader")}
 if _,err:=generated.ConvertSettingCSV(strings.NewReader("limit\n1\n2\n"));err==nil{panic("runtime converter discarded singleton row")}
}
`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", "-mod=mod", ".")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated loader/converter: %v\n%s", err, out)
	}
}

// 指向标量的指针字段（*int32、*string、*Rank 等）：生成的 Convert<Type>CSV 编译得过，结果与生成步骤写出的
// JSON 经 configdata 加载后逐字段相同（横排与 layout=kv 都比）。空单元格的语义以 JSON 为准：生成步骤写零值
// （"bonus": 0，客户端导出同一份 JSON，C# 读成 0 而不是 null），所以是“指向零值的指针”；CSV 里没有这一列 /
// kv 里没有这个 key 时 JSON 没有这个键，指针是 nil。
func TestPointerScalarFieldsInGeneratedConverter(t *testing.T) {
	root := t.TempDir()
	core, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	writeTablegenTestFile(t, filepath.Join(root, "go.mod"), fmt.Sprintf("module example.com/pointer\ngo 1.27.0\nrequire github.com/tjbdwanghaibo/roost-core v0.0.0\nreplace github.com/tjbdwanghaibo/roost-core => %s\n", core))
	writeTablegenTestFile(t, filepath.Join(root, "schema", "item.go"), "package schema\n\ntype Rank int32\n\n"+
		"//roost:table name=item key=ID\ntype Item struct {\n"+
		"\tID    int32   `csv:\"id\" json:\"id\"`\n"+
		"\tBonus *int32  `csv:\"bonus\" json:\"bonus\"`\n"+
		"\tAlias *string `csv:\"alias\" json:\"alias\"`\n"+
		"\tRank  *Rank   `csv:\"rank\" json:\"rank\"`\n"+
		"\tOn    *bool   `csv:\"on\" json:\"on\"`\n"+
		"\tRate  *float64 `csv:\"rate\" json:\"rate\"`\n}\n\n"+
		"//roost:object name=tuning layout=kv\ntype Tuning struct {\n"+
		"\tCap   *int64  `csv:\"cap\" json:\"cap\"`\n"+
		"\tTitle *string `csv:\"title\" json:\"title\"`\n"+
		"\tSeed  *uint32 `csv:\"seed\" json:\"seed\"`\n}\n")
	itemCSV := "id,bonus,alias,rank,on,rate\n1,7,a,2,true,1.5\n2,,,,,\n"
	tuningCSV := "key,value,note\ncap,100,\ntitle,,空值\n"
	writeTablegenTestFile(t, filepath.Join(root, "table", "item.csv"), itemCSV)
	writeTablegenTestFile(t, filepath.Join(root, "table", "tuning.csv"), tuningCSV)
	if err := Run([]string{"-meta", filepath.Join(root, "schema"), "-csv", filepath.Join(root, "table"), "-json", filepath.Join(root, "data"), "-force"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-meta", filepath.Join(root, "schema"), "-out", filepath.Join(root, "generated"), "-pkg", "generated", "-force"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	// 空单元格在 JSON 里是零值，不是 null（客户端导出的也是这一份数据）。
	model, err := ClientExport(filepath.Join(root, "schema"), filepath.Join(root, "table"))
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range model.Tables {
		if table.Name == "item" && !strings.Contains(string(table.Data), `"bonus": 0,`) {
			t.Fatalf("an empty pointer cell is not exported as the zero value:\n%s", table.Data)
		}
	}
	writeTablegenTestFile(t, filepath.Join(root, "main.go"), "package main\n\n"+`import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"

	"example.com/pointer/generated"
	"github.com/tjbdwanghaibo/roost-core/framework/configdata"
)

func check(ok bool, format string, args ...any) {
	if !ok {
		fmt.Printf(format+"\n", args...)
		os.Exit(1)
	}
}

func main() {
	r := configdata.NewRegistry()
	check(generated.RegisterGeneratedConfigData(r) == nil, "register")
	snap, err := configdata.NewStore(r, "data").Load(context.Background())
	check(err == nil, "load: %v", err)
	table, _ := generated.ItemTableFrom(snap)
	loaded := table.Rows()
	raw, _ := os.ReadFile("table/item.csv")
	rows, err := generated.ConvertItemCSV(strings.NewReader(string(raw)))
	check(err == nil && reflect.DeepEqual(rows, loaded), "ConvertItemCSV = %+v, %v; loaded from JSON = %+v", rows, err, loaded)
	check(*rows[0].Bonus == 7 && *rows[0].Alias == "a" && *rows[0].Rank == 2 && *rows[0].On && *rows[0].Rate == 1.5, "row 1 = %+v", rows[0])
	check(rows[1].Bonus != nil && *rows[1].Bonus == 0 && rows[1].Alias != nil && *rows[1].Alias == "" && rows[1].Rank != nil && rows[1].On != nil && !*rows[1].On, "empty cells = %+v", rows[1])
	missing, err := generated.ConvertItemCSV(strings.NewReader("id\n3\n"))
	check(err == nil && len(missing) == 1 && missing[0].Bonus == nil && missing[0].Alias == nil, "missing columns = %+v, %v", missing, err)

	object, _ := generated.TuningConfigFrom(snap)
	raw, _ = os.ReadFile("table/tuning.csv")
	kv, err := generated.ConvertTuningCSV(strings.NewReader(string(raw)))
	check(err == nil && reflect.DeepEqual(kv, object), "ConvertTuningCSV = %+v, %v; loaded from JSON = %+v", kv, err, object)
	check(*kv.Cap == 100 && kv.Title != nil && *kv.Title == "" && kv.Seed == nil, "tuning = %+v", kv)
}
`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", "-mod=mod", ".")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated converter over pointer fields: %v\n%s", err, out)
	}
}
