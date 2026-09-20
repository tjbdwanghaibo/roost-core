package main

import (
	"errors"
	"flag"
	"log"
	"os"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/attribute"
)

func main() {
	if err := attribute.Run(os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}
