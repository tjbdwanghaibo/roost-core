// roost servicerpc generates the bus transport for //roost:rpc interfaces.
package servicerpc

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func Run(args []string, stdout io.Writer) error {
	return RunIn("", args, stdout)
}

// RunIn is Run with relative -dir and -out taken under base instead of the
// process working directory, so a library caller (roost generate / sync)
// never has to os.Chdir the whole process into the tree it generates
// (RR-20261004-12). The flags themselves are kept as given: the generated
// header records them as the command to rerun, and orphan detection matches
// on that header, so "-dir ./internal/rpc/guild" must stay relative. An empty
// base is Run.
func RunIn(base string, args []string, stdout io.Writer) error {
	local := func(path string) string {
		if base == "" || path == "" || filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(base, path)
	}
	flags := flag.NewFlagSet("servicerpc", flag.ContinueOnError)
	flags.SetOutput(stdout)
	dir := flags.String("dir", ".", "package directory to scan for //roost:rpc interfaces")
	// check validates the interfaces AND compares the generated output against
	// what is on disk, writing nothing and failing on any difference.
	//
	// Both halves are needed and the second one was missing. This mode used to
	// return right after the refusals ran, so it exited 0 on a generated file
	// that was stale, hand-edited, or produced by a different version of this
	// generator — while being documented as the CI drift gate. A gate that
	// cannot fail is worse than no gate: it reports that the committed
	// transport matches the interface when nobody checked.
	//
	// The refusals still run first, and that ordering is deliberate: a type
	// that cannot cross a bus faithfully is a design problem, and an author
	// fixing an interface wants that answer rather than a diff.
	check := flags.Bool("check", false, "validate the interfaces and verify the generated files match, writing nothing")
	// emit and out exist for an interface that lives in a different package
	// from its assembly (M-11): the core domain package that owns the interface
	// runs `-emit transport`, and the kit package that assembles it runs
	// `-emit assembly -dir <core package> -out .`, resolving the interface's
	// types through its own aliases.
	emit := flags.String("emit", string(HalfAll), "which half to emit: transport, assembly or all")
	out := flags.String("out", "", "directory to write into (default: -dir)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	half := Half(*emit)
	switch half {
	case HalfAll, HalfTransport, HalfAssembly:
	default:
		return fmt.Errorf("-emit %q: want transport, assembly or all", *emit)
	}
	outDir := ""
	if *out != "" {
		var err error
		if outDir, err = filepath.Abs(local(*out)); err != nil {
			return fmt.Errorf("resolve out: %w", err)
		}
	}
	dirPath := *dir
	if info, statErr := os.Stat(local(dirPath)); statErr == nil && info.IsDir() {
		dirPath = local(dirPath)
	}
	moduleDir := outDir
	if moduleDir == "" {
		moduleDir = base
	}
	absDir, err := resolveDir(dirPath, moduleDir)
	if err != nil {
		return err
	}
	if outDir == "" {
		outDir = absDir
	}
	regenerate := regenerateCommand(*dir, *out, half)
	services, err := ParseDir(absDir)
	if err != nil {
		return err
	}
	expected := make(map[string]bool)
	var generated []File
	for _, service := range services {
		files, err := GenerateWith(service, Options{Half: half, Regenerate: regenerate})
		if err != nil {
			return err
		}
		for _, file := range files {
			expected[file.Name] = true
			generated = append(generated, file)
		}
	}
	orphans, err := orphanGeneratedFiles(outDir, regenerate, half, expected)
	if err != nil {
		return err
	}
	if len(services) == 0 {
		_, _ = fmt.Fprintf(stdout, "no //roost:rpc interfaces in %s\n", absDir)
	}
	for _, service := range services {
		_, _ = fmt.Fprintf(stdout, "%s.%s: service_type=%s capability=%s methods=%d\n",
			service.Package, service.Interface, service.ServiceType, service.Capability,
			len(service.Methods))
		for _, method := range service.Methods {
			suffix := ""
			if method.Affinity != "" {
				suffix += fmt.Sprintf(" affinity=%s", method.Affinity)
			}
			if method.Reliable {
				suffix += " reliable"
			}
			_, _ = fmt.Fprintf(stdout, "  %-14s params=%d results=%d%s\n",
				method.Name, len(method.Params), len(method.Results), suffix)
		}
	}
	var stale []string
	for _, file := range generated {
		path := filepath.Join(outDir, file.Name)
		existing, readErr := os.ReadFile(path)
		// A CRLF checkout of the LF the generator writes is current, not
		// stale: newlines are the working tree's business, not the
		// transport's.
		current := readErr == nil && bytes.Equal(bytes.ReplaceAll(existing, []byte("\r\n"), []byte("\n")), file.Content)
		if current {
			_, _ = fmt.Fprintf(stdout, "up to date: %s\n", file.Name)
			continue
		}
		if *check {
			// Missing and differing are reported apart: one means nobody ran
			// the generator, the other means the file was edited or produced
			// by a different version of it, and the fix is not the same.
			if readErr != nil {
				stale = append(stale, file.Name+" (missing)")
			} else {
				stale = append(stale, file.Name)
			}
			_, _ = fmt.Fprintf(stdout, "STALE: %s\n", file.Name)
			continue
		}
		if err := os.WriteFile(path, file.Content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		_, _ = fmt.Fprintf(stdout, "generated: %s\n", file.Name)
	}
	for _, name := range orphans {
		if *check {
			stale = append(stale, name+" (orphan)")
			_, _ = fmt.Fprintf(stdout, "STALE: %s (orphan)\n", name)
			continue
		}
		if err := os.Remove(filepath.Join(outDir, name)); err != nil {
			return fmt.Errorf("remove orphan %s: %w", name, err)
		}
		_, _ = fmt.Fprintf(stdout, "removed orphan: %s\n", name)
	}
	if len(stale) > 0 {
		return fmt.Errorf("%s: generated transport does not match the current interfaces: %s. "+
			"Run `go generate ./...` and commit the result — a hand-edited generated file is "+
			"reverted by the next run, and a file produced by a different version of this "+
			"generator means the committed transport is not the one this interface describes",
			outDir, strings.Join(stale, ", "))
	}
	return nil
}

// orphanGeneratedFiles only owns the selected half made by this exact
// invocation. A kit output directory may also contain assembly files from
// other core packages; their recorded regenerate commands must not match.
func orphanGeneratedFiles(outDir, regenerate string, half Half, expected map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(outDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan generated RPC files in %s: %w", outDir, err)
	}
	var orphans []string
	for _, entry := range entries {
		name := entry.Name()
		transport := strings.HasSuffix(name, "_rpc_gen.go") && half != HalfAssembly
		assembly := strings.HasSuffix(name, "_rpc_assembly_gen.go") && half != HalfTransport
		if !entry.Type().IsRegular() || expected[name] || (!transport && !assembly) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			return nil, fmt.Errorf("inspect generated RPC file %s: %w", name, err)
		}
		if !bytes.HasPrefix(raw, []byte("// Code generated by roost servicerpc. DO NOT EDIT.")) {
			continue
		}
		normalized := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
		if bytes.Contains(normalized, []byte("//\t"+regenerate+"\n")) {
			orphans = append(orphans, name)
		}
	}
	return orphans, nil
}

// regenerateCommand is what the generated header tells a reader to run: the
// flags as given, so a file generated from another package's interface
// records where that interface is.
func regenerateCommand(dir, out string, half Half) string {
	cmd := "go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/servicerpc -dir " + dir
	if half != HalfAll {
		cmd += " -emit " + string(half)
	}
	if out != "" {
		cmd += " -out " + out
	}
	return cmd
}

// resolveDir turns -dir into a directory. A path on disk is taken as is. An
// import path — nothing on disk by that name, and shaped like one (a dotted
// first segment) — is resolved with `go list` in the module context of
// moduleDir (the -out directory, else the working directory), so a kit package
// can generate its assembly half from an interface that lives in the core
// module without spelling the module cache path (M-11).
func resolveDir(dir, moduleDir string) (string, error) {
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("resolve dir: %w", err)
		}
		return abs, nil
	}
	if !looksLikeImportPath(dir) {
		return "", fmt.Errorf("-dir %q: not a directory", dir)
	}
	cmd := exec.Command("go", "list", "-f", "{{.Dir}}", dir)
	cmd.Dir = moduleDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	outBytes, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("-dir %q: go list: %w\n%s", dir, err, stderr.String())
	}
	resolved := strings.TrimSpace(string(outBytes))
	if resolved == "" {
		return "", fmt.Errorf("-dir %q: go list returned no directory", dir)
	}
	return resolved, nil
}

// looksLikeImportPath is the shape test go itself uses for a module path:
// the first element has a dot in it.
func looksLikeImportPath(s string) bool {
	first, _, _ := strings.Cut(s, "/")
	return strings.Contains(first, ".") && !strings.HasPrefix(s, ".") && !filepath.IsAbs(s)
}
