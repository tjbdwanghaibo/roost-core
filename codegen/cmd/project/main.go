package main

import (
	"errors"
	"flag"
	"log"
	"os"

	"github.com/tjbdwanghaibo/roost-codegen/internal/roost"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] != "new" && args[0] != "sync" && args[0] != "diff" && args[0] != "doctor" && args[0] != "upgrade" && args[0] != "deps" {
		args = append([]string{"new"}, args...)
	}
	args = append([]string{"project"}, args...)
	if err := roost.Run(args, os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}
