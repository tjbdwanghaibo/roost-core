package configdata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

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
//   - Every byte the callback reads is folded into a content fingerprint
//     that feeds Snapshot.Hash, so external table drift stays visible even
//     though the aggregate's own state is unexported (json.Marshal would
//     see an empty object). The callback must therefore read its files in a
//     deterministic order.
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
func RegisterExternalTables[T any](r *Registry, name Name, build func(read func(file string) ([]byte, error)) (T, error)) error {
	if build == nil {
		return fmt.Errorf("configdata: external tables %s: build callback is required", name)
	}
	return RegisterCustom(r, CustomDef[T]{
		Name: name,
		Build: func(ctx *BuildContext) (T, error) {
			dir := ctx.Dir // one consistent directory for the whole build
			hasher := sha256.New()
			closed := false
			read := func(file string) ([]byte, error) {
				if closed {
					return nil, fmt.Errorf("configdata: external tables %s: read called after build returned (lazy loading is not supported)", name)
				}
				if file == "" || !filepath.IsLocal(file) {
					return nil, fmt.Errorf("configdata: external tables %s: file %q escapes the data directory", name, file)
				}
				raw, err := os.ReadFile(filepath.Join(dir, file))
				if err != nil {
					return nil, err
				}
				_, _ = fmt.Fprintf(hasher, "%d:%s%d:", len(file), file, len(raw))
				_, _ = hasher.Write(raw)
				return raw, nil
			}
			value, err := safeExternalBuild(name, build, read)
			closed = true
			if err != nil {
				var zero T
				return zero, err
			}
			ctx.Snapshot.SetFingerprint(name, hex.EncodeToString(hasher.Sum(nil)))
			return value, nil
		},
	})
}

func safeExternalBuild[T any](name Name, build func(func(string) ([]byte, error)) (T, error), read func(string) ([]byte, error)) (value T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("configdata: external tables %s: build panicked: %v", name, r)
		}
	}()
	value, err = build(read)
	if err != nil {
		err = fmt.Errorf("configdata: external tables %s: %w", name, err)
	}
	return value, err
}

// MustRegisterExternalTables is RegisterExternalTables panicking on error.
func MustRegisterExternalTables[T any](r *Registry, name Name, build func(read func(file string) ([]byte, error)) (T, error)) {
	if err := RegisterExternalTables(r, name, build); err != nil {
		panic(err)
	}
}

// ExternalTablesFrom returns an aggregate registered via
// RegisterExternalTables from a snapshot.
func ExternalTablesFrom[T any](snap *Snapshot, name Name) (T, bool) {
	return CustomFrom[T](snap, name)
}
