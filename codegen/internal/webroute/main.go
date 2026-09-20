// roost webroute generates HTTP route registrations from //roost:web markers.
package webroute

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
)

func Run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("webroute", flag.ContinueOnError)
	flags.SetOutput(stdout)
	dir := flags.String("dir", ".", "directory to scan recursively for //roost:web markers")
	force := flags.Bool("force", false, "regenerate files even when unchanged")
	if err := flags.Parse(args); err != nil {
		return err
	}

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}
	changed, err := GenerateDir(absDir, *force)
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		_, _ = fmt.Fprintln(stdout, "all files up to date")
		return nil
	}
	for _, path := range changed {
		_, _ = fmt.Fprintf(stdout, "generated: %s\n", path)
	}
	return nil
}
