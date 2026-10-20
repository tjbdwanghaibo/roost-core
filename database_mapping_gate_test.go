package roostcore_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// DAO 的 db= 是逻辑库名，物理库名只能经 DataEngine 冻结的映射表（dataengine.DatabaseMap.Physical）得到
// （DESIGN-TODO D9 修订版 A）。这条守卫在产品代码与生成模板里找“把逻辑库名直接交给 Mongo 打开库”的写法：
// .Database( / .DatabaseForSid( 的参数里出现 DbName()、<Dao>DBName、db.Database<名字> 常量，或 DocumentKey /
// Remote mutation 的 .Database 字段，而没有经过 .Physical(。
//
// 保证与限制：只看同一行里的调用参数（Go 源码与 .tmpl 模板逐行匹配）；先赋给局部变量再传入的写法看不到，
// 由各解析点自己的行为测试兜底（投影、冷加载、Remote 存储、工具）。测试代码与 testdata 不在范围内：测试
// 夹具不配映射时物理名就是逻辑名，按逻辑名清库是有意的。
var logicalDatabaseAsPhysical = regexp.MustCompile(`\.Database(?:ForSid)?\(([^()]*(?:\([^()]*\)[^()]*)*)\)`)

var logicalDatabaseOperand = regexp.MustCompile(`DbName\(\)|DBName\b|\bdb\.Database[A-Z]\w*|Key\.Database\b|(?:mutation|item|m|key)\.Database\b`)

func TestNoMongoDatabaseOpenedWithALogicalDAOName(t *testing.T) {
	roots := []string{"framework", "wiring", "service", "gameplay", "demo", "cmd", "codegen/internal"}
	var violations []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			name := entry.Name()
			isGo := strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
			isTemplate := strings.HasSuffix(name, ".go.tmpl") && !strings.HasSuffix(name, "_test.go.tmpl")
			if !isGo && !isTemplate {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for number, line := range strings.Split(string(raw), "\n") {
				for _, call := range logicalDatabaseAsPhysical.FindAllStringSubmatch(line, -1) {
					argument := call[1]
					if strings.Contains(argument, ".Physical(") || !logicalDatabaseOperand.MatchString(argument) {
						continue
					}
					violations = append(violations, path+":"+strconv.Itoa(number+1)+": "+strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, violation := range violations {
		t.Errorf("opens a Mongo database with a logical DAO database name; resolve it through the DataEngine database map (DatabaseMap.Physical, kitdataengine.DAOCollection): %s", violation)
	}
}
