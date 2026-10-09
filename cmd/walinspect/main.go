// walinspect检查停机WAL并可创建完整隔离副本，从不跳过记录或推进checkpoint。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
)

func main() {
	var opts nestwal.InspectOptions
	flag.StringVar(&opts.Dir, "dir", "", "existing stopped WAL directory (required)")
	flag.StringVar(&opts.SnapshotDir, "snapshot", "", "new directory for complete copy, hashes and report")
	flag.IntVar(&opts.MaxRecordBytes, "max-record-bytes", 0, "record limit; zero uses WAL default")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	report, err := nestwal.Inspect(ctx, opts)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if outputErr := encoder.Encode(report); outputErr != nil {
		fmt.Fprintln(os.Stderr, outputErr)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
