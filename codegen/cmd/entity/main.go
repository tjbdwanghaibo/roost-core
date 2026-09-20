package main

import (
	"errors"
	"flag"
	"log"
	"os"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/entity"
)

func main() {
	if err := entity.Run(os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}
