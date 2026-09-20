// Command attributeruntime prints the attribute feature's runtime file for a
// package name. It exists so scripts/attribute-runtime.sh compiles exactly
// what the project scaffold writes, rather than a copy that can drift.
package main

import (
	"fmt"
	"os"

	"github.com/tjbdwanghaibo/roost-codegen/internal/roost"
)

func main() {
	pkg := "attribute"
	if len(os.Args) > 1 {
		pkg = os.Args[1]
	}
	fmt.Print(roost.AttributeRuntimeFile(pkg))
}
