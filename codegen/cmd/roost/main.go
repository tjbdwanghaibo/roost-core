package main

import (
	"errors"
	"flag"
	"log"
	"os"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/roost"
)

func main() {
	if err := roost.Main(os.Args[1:], os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		// 区分结论的命令（roost config check）带自己的退出码，细节已经打在 stdout 时只给一行摘要；其余失败是 1。
		var summarized interface{ ExitSummary() string }
		if errors.As(err, &summarized) && summarized.ExitSummary() != "" {
			log.Print(summarized.ExitSummary())
		} else {
			log.Print(err)
		}
		var coded interface{ ExitCode() int }
		if errors.As(err, &coded) {
			os.Exit(coded.ExitCode())
		}
		os.Exit(1)
	}
}
