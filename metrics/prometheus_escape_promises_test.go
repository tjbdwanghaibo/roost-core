package metrics

import (
	"strings"
	"testing"
)

// RR-20261005-NC-264（N12 观察 O4）：Prometheus 文本格式的标签值只有三种转义：`\\`、`\"`、`\n`，
// 其余字符（含制表符与非 ASCII）原样写出，内容须是合法 UTF-8。旧实现用 strconv.Quote：制表符写成 `\t`、
// 不可打印字符写成 `\u....`、非法 UTF-8 写成 `\x..`——这些都不在 exposition 格式的转义集合里，
// 抓取器要么整页解析失败（invalid escape sequence），要么把标签值读成另一个字符串。
func TestPrometheusLabelValuesUseTheExpositionEscapes(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"plain", "game-1", `v="game-1"`},
		{"quote backslash newline", "a\"b\\c\nd", `v="a\"b\\c\nd"`},
		{"tab stays raw", "a\tb", "v=\"a\tb\""},
		{"non-ascii stays raw", "区服​1", "v=\"区服​1\""},
		{"invalid utf-8 is replaced", "a\xffb", "v=\"a�b\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(PrometheusText([]Metric{{Name: "m", Kind: KindGauge, Labels: Labels{"v": tc.value}, Value: 1}}))
			want := "m{" + tc.want + "} 1\n"
			if out != want {
				t.Errorf("exposition = %q, want %q", out, want)
			}
			if strings.Contains(out, `\t`) || strings.Contains(out, `\x`) || strings.Contains(out, `\u`) {
				t.Errorf("exposition %q uses an escape the text format does not define", out)
			}
		})
	}
}
