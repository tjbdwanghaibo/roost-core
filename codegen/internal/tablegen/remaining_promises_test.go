package tablegen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

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
import("context";"strings";"example.com/tablecheck/generated";"github.com/tjbdwanghaibo/roost-core/configdata")
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
