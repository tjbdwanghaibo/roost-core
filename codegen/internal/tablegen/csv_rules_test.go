package tablegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCSV(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monster.csv")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func monsterMeta() Meta {
	return Meta{Kind: KindTable, Name: "monster", File: "monster.csv", Key: "ID", TypeName: "Monster", Fields: []Field{
		{Name: "ID", Type: "int32", CSV: "id", JSON: "id", Title: "编号", Required: true},
		{Name: "Name", Type: "string", CSV: "name", JSON: "name", Title: "名字", Required: true},
		{Name: "Level", Type: "int32", CSV: "level", JSON: "level", Title: "等级", Min: "1"},
		{Name: "Code", Type: "string", CSV: "code", JSON: "code", Title: "代号", Unique: true},
	}}
}

// The rules a table declares in its tags are enforced when the CSV is turned
// into JSON — the moment the author can still fix the sheet. Each case breaks
// one rule; the error names the file, the field and the row.
func TestReadCSVRecordsEnforcesDeclaredRules(t *testing.T) {
	cases := []struct{ label, csv, want string }{
		{"required cell empty", "id,name,level,code\n1,,3,a\n", "required field is empty"},
		{"unparsable int", "id,name,level,code\n1,slime,abc,a\n", "row=2 col=level field=Level"},
		{"key repeated", "id,name,level,code\n1,slime,3,a\n1,orc,4,b\n", "key field ID repeats value \"1\" in data rows 1 and 2"},
		{"unique column repeated", "id,name,level,code\n1,slime,3,a\n2,orc,4,a\n", "unique field Code repeats value \"a\""},
		{"below min", "id,name,level,code\n1,slime,0,a\n", "below min=1"},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			_, err := readCSVRecords(writeCSV(t, testCase.csv), monsterMeta())
			if err == nil {
				t.Fatalf("accepted a sheet that breaks %q", testCase.label)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not contain %q", err, testCase.want)
			}
		})
	}
}

// The optional title / type / rule rows the generator itself emits into a
// sheet are recognised and skipped, so a round-tripped CSV does not parse its
// own header rows as data.
func TestReadCSVRecordsSkipsTitleTypeAndRuleRows(t *testing.T) {
	meta := monsterMeta()
	titles := "编号,名字,等级,代号\n"
	types := "int32,string,int32,string\n"
	rules := strings.Join(fieldValues(meta.Fields, fieldRule), ",") + "\n"
	rows, err := readCSVRecords(writeCSV(t, "id,name,level,code\n"+titles+types+rules+"1,slime,3,a\n2,orc,4,b\n"), meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0]["name"] != "slime" || rows[1]["level"] != int64(4) {
		t.Fatalf("rows = %#v", rows)
	}
}

// A generated file is never overwritten without -force: the check exists so a
// stale generator cannot clobber a hand-edited output by accident.
func TestWriteGeneratedRefusesToOverwriteWithoutForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.go")
	if err := writeGenerated(path, []byte("one"), false); err != nil {
		t.Fatal(err)
	}
	if err := writeGenerated(path, []byte("two"), false); err == nil || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("second write without -force: err=%v", err)
	}
	if err := writeGenerated(path, []byte("two"), true); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "two" {
		t.Fatalf("content after -force = %q", raw)
	}
}
