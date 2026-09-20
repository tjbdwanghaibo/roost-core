package main

import (
	"errors"
	"flag"
	"log"
	"os"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/roost"
)

func main() {
	if err := roost.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}
