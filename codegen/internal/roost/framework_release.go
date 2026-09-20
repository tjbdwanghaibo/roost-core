package roost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// FrameworkReleaseManifest declares the release being promoted.
//
// Schema 3 has ONE version because the framework is one module: kit and the
// generator ship inside roost-core (kit/, codegen/). Schema 2 listed codegen
// separately from framework.core / framework.kit, and keeping three numbers in
// step was itself a source of failure — the codegen field drifted from the tag
// and took ten releases' worth of this gate with it (U-0270).
type FrameworkReleaseManifest struct {
	Schema     int      `yaml:"schema"`
	Release    string   `yaml:"release"`
	ConsumerGo []string `yaml:"consumer_go"`
}

type FrameworkModuleLock struct {
	Path     string `json:"path"`
	Version  string `json:"version"`
	Sum      string `json:"sum"`
	GoModSum string `json:"go_mod_sum"`
}

type FrameworkReleaseLock struct {
	Schema     int                   `json:"schema"`
	Release    string                `json:"release"`
	ConsumerGo []string              `json:"consumer_go"`
	Modules    []FrameworkModuleLock `json:"modules"`
}

type moduleDownload struct {
	Path     string
	Version  string
	GoMod    string
	Sum      string
	GoModSum string
	Error    *struct{ Err string }
}

var internalPseudoVersion = regexp.MustCompile(`github\.com/tjbdwanghaibo/roost-core\s+v\d+\.\d+\.\d+-0\.\d{14}-[0-9a-f]{12}`)
var goModReplaceDirective = regexp.MustCompile(`(?m)^[\t ]*replace(?:[\t ]|\()`)
var consumerGoVersion = regexp.MustCompile(`^1\.(\d+)\.x$`)

func LoadFrameworkReleaseManifest(path string) (FrameworkReleaseManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return FrameworkReleaseManifest{}, fmt.Errorf("read framework release manifest: %w", err)
	}
	var manifest FrameworkReleaseManifest
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return FrameworkReleaseManifest{}, fmt.Errorf("decode framework release manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return FrameworkReleaseManifest{}, err
	}
	return manifest, nil
}

func (m FrameworkReleaseManifest) Validate() error {
	var joined error
	if m.Schema != 3 {
		joined = errors.Join(joined, fmt.Errorf("unsupported framework release schema %d (schema 3 names one module)", m.Schema))
	}
	if _, _, _, ok := releaseVersion(m.Release); !ok {
		joined = errors.Join(joined, fmt.Errorf("release must be an exact vMAJOR.MINOR.PATCH release; got %q", m.Release))
	}
	seenGo := make(map[string]bool, len(m.ConsumerGo))
	if len(m.ConsumerGo) == 0 {
		joined = errors.Join(joined, errors.New("consumer_go must contain at least one supported Go release line"))
	}
	for _, version := range m.ConsumerGo {
		match := consumerGoVersion.FindStringSubmatch(version)
		if len(match) != 2 {
			joined = errors.Join(joined, fmt.Errorf("consumer Go version must use 1.MINOR.x syntax; got %q", version))
		} else if minor, err := strconv.Atoi(match[1]); err != nil || minor < 25 {
			joined = errors.Join(joined, fmt.Errorf("consumer Go version must be 1.25.x or newer; got %q", version))
		}
		if seenGo[version] {
			joined = errors.Join(joined, fmt.Errorf("duplicate consumer Go version %q", version))
		}
		seenGo[version] = true
	}
	return joined
}

func VerifyFrameworkRelease(manifestPath, expectedRelease, lockPath, githubOutput string, stdout io.Writer) error {
	manifest, err := LoadFrameworkReleaseManifest(manifestPath)
	if err != nil {
		return err
	}
	if expectedRelease != "" && manifest.Release != expectedRelease {
		return fmt.Errorf("framework release %s does not match release tag %s", manifest.Release, expectedRelease)
	}
	modules := []struct{ path, version string }{
		{"github.com/tjbdwanghaibo/roost-core", manifest.Release},
	}
	lock := FrameworkReleaseLock{Schema: 3, Release: manifest.Release, ConsumerGo: append([]string(nil), manifest.ConsumerGo...)}
	for _, module := range modules {
		download, downloadErr := downloadFrameworkModule(module.path, module.version)
		if downloadErr != nil {
			return downloadErr
		}
		goMod, readErr := os.ReadFile(download.GoMod)
		if readErr != nil {
			return fmt.Errorf("read %s@%s go.mod: %w", module.path, module.version, readErr)
		}
		if err := validatePublishedFrameworkGoMod(module.path, module.version, goMod); err != nil {
			return err
		}
		lock.Modules = append(lock.Modules, FrameworkModuleLock{Path: download.Path, Version: download.Version, Sum: download.Sum, GoModSum: download.GoModSum})
	}
	if lockPath != "" {
		raw, marshalErr := json.MarshalIndent(lock, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		raw = append(raw, '\n')
		if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
			return fmt.Errorf("create framework release lock directory: %w", err)
		}
		if err := writeAtomic(lockPath, raw, 0o644); err != nil {
			return fmt.Errorf("write framework release lock: %w", err)
		}
	}
	if githubOutput != "" {
		if err := appendFrameworkGitHubOutput(githubOutput, manifest); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "framework release verified: %s\n", manifest.Release)
	return nil
}

func validatePublishedFrameworkGoMod(path, version string, goMod []byte) error {
	if goModReplaceDirective.Match(goMod) {
		return fmt.Errorf("published module %s@%s contains a replace directive", path, version)
	}
	if dependency := internalPseudoVersion.Find(goMod); dependency != nil {
		return fmt.Errorf("published module %s@%s depends on an unpublished framework pseudo-version: %s", path, version, dependency)
	}
	return nil
}

func downloadFrameworkModule(path, version string) (moduleDownload, error) {
	command := exec.Command("go", "mod", "download", "-json", path+"@"+version)
	raw, err := command.CombinedOutput()
	if err != nil {
		return moduleDownload{}, fmt.Errorf("download %s@%s: %w\n%s", path, version, err, raw)
	}
	var result moduleDownload
	if err := json.Unmarshal(raw, &result); err != nil {
		return moduleDownload{}, fmt.Errorf("decode module download %s@%s: %w", path, version, err)
	}
	if result.Error != nil {
		return moduleDownload{}, fmt.Errorf("download %s@%s: %s", path, version, result.Error.Err)
	}
	if result.Path != path || result.Version != version || result.GoMod == "" || result.Sum == "" || result.GoModSum == "" {
		return moduleDownload{}, fmt.Errorf("incomplete or mismatched module metadata for %s@%s", path, version)
	}
	return result, nil
}

func appendFrameworkGitHubOutput(path string, manifest FrameworkReleaseManifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open GitHub output: %w", err)
	}
	defer file.Close()
	consumerGoJSON, err := json.Marshal(manifest.ConsumerGo)
	if err != nil {
		return fmt.Errorf("encode consumer Go matrix: %w", err)
	}
	values := []string{
		"release=" + manifest.Release,
		"consumer_go=" + strings.Join(manifest.ConsumerGo, ","),
		"consumer_go_json=" + string(consumerGoJSON),
	}
	if _, err := io.WriteString(file, strings.Join(values, "\n")+"\n"); err != nil {
		return fmt.Errorf("write GitHub output: %w", err)
	}
	return file.Sync()
}
