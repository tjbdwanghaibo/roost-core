package attribute

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
)

func Run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("attribute", flag.ContinueOnError)
	flags.SetOutput(stdout)
	dir := flags.String("dir", ".", "directory to scan")
	output := flags.String("output", "", "output file (default: gen_<profile>_attribute.go)")
	force := flags.Bool("force", false, "force regeneration")
	if err := flags.Parse(args); err != nil {
		return err
	}

	scanDir, err := filepath.Abs(*dir)
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}
	profiles, err := parseDir(scanDir)
	if err != nil {
		return fmt.Errorf("parse attribute profiles: %w", err)
	}
	if len(profiles) == 0 {
		_, _ = fmt.Fprintf(stdout, "no attribute markers found in %s\n", scanDir)
		return nil
	}
	for _, profile := range profiles {
		outFile := *output
		if outFile == "" {
			outFile = filepath.Join(scanDir, fmt.Sprintf("gen_%s_attribute.go", toSnake(profile.Name)))
		}
		content, err := generate(profile)
		if err != nil {
			return fmt.Errorf("generate %s: %w", profile.Name, err)
		}
		changed, err := writeIfChanged(outFile, content, *force)
		if err != nil {
			return fmt.Errorf("write %s: %w", outFile, err)
		}
		if changed {
			_, _ = fmt.Fprintf(stdout, "generated: %s\n", outFile)
		} else {
			_, _ = fmt.Fprintf(stdout, "unchanged: %s\n", outFile)
		}
	}
	return nil
}
