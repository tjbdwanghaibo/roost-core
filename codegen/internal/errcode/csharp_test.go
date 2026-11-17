package errcode

import (
	"strings"
	"testing"
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
