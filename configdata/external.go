package configdata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// ExternalOption customizes RegisterExternalTables.
type ExternalOption[T any] func(*externalConfig[T])

type externalConfig[T any] struct {
	validate func(*BuildContext, T) error
}

// WithExternalValidate adds a validation callback for the built aggregate —
// the place for "key tables must be non-empty" style invariants that
// configdata cannot see into an opaque external value.
func WithExternalValidate[T any](validate func(*BuildContext, T) error) ExternalOption[T] {
	return func(c *externalConfig[T]) { c.validate = validate }
}

// RegisterExternalTables registers an externally generated table aggregate —
// typically the Tables object emitted by Luban's code_go_json target — as
// one snapshot member. The build callback receives a file reader rooted at
// the store's data directory and is re-run on every load/reload, so the
// aggregate inherits configdata's runtime semantics wholesale: atomic
// hot-reload, rollback, content hash, and in-flight request consistency via
// ActiveSnapshot. The external toolchain keeps what it is good at (schema,
// validation, export, codegen); this hook keeps the runtime ours.
//
// Contracts:
//
//   - Every file the callback reads is folded into an order-independent
//     content fingerprint (per-file digests combined in sorted file order)
//     that feeds Snapshot.Hash, so external table drift stays visible even
//     though the aggregate's own state is unexported. Parallel reads inside
//     the callback are safe and do not change the fingerprint.
//
//   - A callback that reads nothing through the injected reader fails the
//     build: bypassing the reader (os.ReadFile inside generated code) would
//     silently freeze the fingerprint and hide all drift.
//
//   - A panicking callback (generated loaders choke on malformed data by
//     panicking) is converted into a build error: the reload fails and the
//     previous snapshot stays live, instead of the process crashing.
//
//   - The read function is only valid during the callback; calling it after
//     the callback returns yields an error (lazy loaders would bypass both
//     the fingerprint and the fail-fast semantics).
//
//     configdata.MustRegisterExternalTables(reg, "luban", func(read func(string) ([]byte, error)) (*cfg.Tables, error) {
//     return cfg.NewTables(func(file string) ([]byte, error) { return read(file + ".json") })
//     })
//     ...
//     tables, ok := configdata.ExternalTablesFrom[*cfg.Tables](snap, "luban")
func RegisterExternalTables[T any](r *Registry, name Name, build func(read func(file string) ([]byte, error)) (T, error), opts ...ExternalOption[T]) error {
	if build == nil {
		return fmt.Errorf("configdata: external tables %s: build callback is required", name)
	}
	var cfg externalConfig[T]
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return RegisterCustom(r, CustomDef[T]{
		Name:     name,
		Validate: cfg.validate,
		Build: func(ctx *BuildContext) (T, error) {
			var zero T
			dir := ctx.Dir // one consistent directory for the whole build
			resolvedDir, err := filepath.EvalSymlinks(dir)
			if err != nil {
				return zero, fmt.Errorf("configdata: external tables %s: resolve data dir: %w", name, err)
			}
			var (
				mu     sync.Mutex
				files  = make(map[string][sha256.Size]byte)
				closed atomic.Bool
			)
			read := func(file string) ([]byte, error) {
				if closed.Load() {
					return nil, fmt.Errorf("configdata: external tables %s: read called after build returned (lazy loading is not supported)", name)
				}
				if !filepath.IsLocal(file) {
					return nil, fmt.Errorf("configdata: external tables %s: file %q escapes the data directory", name, file)
				}
				path := filepath.Join(dir, file)
				// Lexical checks don't see symlinks: resolve and re-verify
				// the real path still lives under the data directory.
				resolved, err := filepath.EvalSymlinks(path)
				if err != nil {
					return nil, err
				}
				if resolved != resolvedDir && !strings.HasPrefix(resolved, resolvedDir+string(filepath.Separator)) {
					return nil, fmt.Errorf("configdata: external tables %s: file %q resolves outside the data directory", name, file)
				}
				raw, err := os.ReadFile(resolved)
				if err != nil {
					return nil, err
				}
				digest := sha256.Sum256(raw)
				mu.Lock()
				files[file] = digest // re-reads overwrite: last content wins
				mu.Unlock()
				return raw, nil
			}
			value, err := safeExternalBuild(name, build, read)
			closed.Store(true)
			if err != nil {
				return zero, err
			}
			mu.Lock()
			defer mu.Unlock()
			if len(files) == 0 {
				return zero, fmt.Errorf("configdata: external tables %s: build callback read no files through the injected reader — the content fingerprint would be constant and drift invisible", name)
			}
			// Order-independent fingerprint: per-file digests folded in
			// sorted file order, so parallel or reordered reads inside the
			// callback cannot flip the hash.
			names := make([]string, 0, len(files))
			for file := range files {
				names = append(names, file)
			}
			sort.Strings(names)
			hasher := sha256.New()
			for _, file := range names {
				digest := files[file]
				_, _ = fmt.Fprintf(hasher, "%d:%s", len(file), file)
				_, _ = hasher.Write(digest[:])
			}
			ctx.Snapshot.SetFingerprint(name, hex.EncodeToString(hasher.Sum(nil)))
			return value, nil
		},
	})
}

func safeExternalBuild[T any](name Name, build func(func(string) ([]byte, error)) (T, error), read func(string) ([]byte, error)) (value T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = wrapPanic(fmt.Sprintf("configdata: external tables %s: build", name), r)
		}
	}()
	value, err = build(read)
	if err != nil {
		err = fmt.Errorf("configdata: external tables %s: %w", name, err)
	}
	return value, err
}

// MustRegisterExternalTables is RegisterExternalTables panicking on error.
func MustRegisterExternalTables[T any](r *Registry, name Name, build func(read func(file string) ([]byte, error)) (T, error), opts ...ExternalOption[T]) {
	if err := RegisterExternalTables(r, name, build, opts...); err != nil {
		panic(err)
	}
}

// ExternalTablesFrom returns an aggregate registered via
// RegisterExternalTables from a snapshot.
func ExternalTablesFrom[T any](snap *Snapshot, name Name) (T, bool) {
	return CustomFrom[T](snap, name)
}
