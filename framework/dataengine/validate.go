package dataengine

import (
	"errors"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

var (
	ErrInvalidDocumentKey  = errors.New("dataengine: invalid document key")
	ErrInvalidMutationKind = errors.New("dataengine: invalid mutation kind")
	ErrInvalidVersion      = errors.New("dataengine: invalid mutation version")
	ErrInvalidPut          = errors.New("dataengine: invalid put mutation")
	ErrInvalidPatch        = errors.New("dataengine: invalid patch mutation")
	ErrInvalidDelete       = errors.New("dataengine: invalid delete mutation")
	ErrInvalidPatchPath    = errors.New("dataengine: invalid patch path")
)

// ValidateMutation 在 WAL 准入前检查单条变更的身份、版本链与内容形状。
// Remote 变更只允许 Put/Delete 且不带本地内容，版本必须与远端提交一致。
func ValidateMutation(m Mutation) error {
	if m.Key.ID == 0 || m.Key.Resource == "" || (m.Key.Database == "" && m.Remote == nil) {
		return ErrInvalidDocumentKey
	}
	if m.NextVersion == 0 || m.NextVersion != m.ExpectedVersion+1 {
		return ErrInvalidVersion
	}
	if m.Remote != nil {
		if err := m.Remote.Validate(); err != nil {
			return fmt.Errorf("dataengine: invalid remote mutation: %w", err)
		}
		if m.Remote.EntityID != m.Key.ID || m.Remote.BaseVersion != m.ExpectedVersion || m.Remote.NextVersion != m.NextVersion {
			return ErrInvalidVersion
		}
		if (m.Remote.Delete && m.Kind != MutationDelete) || (!m.Remote.Delete && m.Kind != MutationPut) || len(m.Data) != 0 || !m.Patch.Empty() {
			return ErrInvalidMutationKind
		}
		return nil
	}

	switch m.Kind {
	case MutationPut:
		if len(m.Data) == 0 || !m.Patch.Empty() {
			return ErrInvalidPut
		}
	case MutationPatch:
		if m.ExpectedVersion == 0 || len(m.Data) != 0 || m.Patch.Empty() {
			return ErrInvalidPatch
		}
	case MutationDelete:
		if len(m.Data) != 0 || !m.Patch.Empty() {
			return ErrInvalidDelete
		}
	default:
		return ErrInvalidMutationKind
	}
	return validatePatchPaths(m.Patch)
}

// ValidateCommitRecord 检查事务 ID、每条变更、effect 与回执。
func ValidateCommitRecord(record CommitRecord) error {
	if record.ID.IsZero() {
		return errors.New("dataengine: zero transaction id")
	}
	for i := range record.Mutations {
		if err := ValidateMutation(record.Mutations[i]); err != nil {
			return fmt.Errorf("dataengine: invalid mutation %d: %w", i, err)
		}
	}
	for i, effect := range record.Effects {
		if effect.ID == "" || effect.Topic == "" {
			return fmt.Errorf("dataengine: invalid effect %d", i)
		}
	}
	for i, receipt := range record.Receipts {
		if receipt.Namespace == "" || receipt.ID == "" {
			return fmt.Errorf("dataengine: invalid receipt %d", i)
		}
	}
	return nil
}

func validatePatchPaths(patch FieldPatch) error {
	paths := make(map[string]struct{}, len(patch.Unset))
	add := func(path string) error {
		if !validPatchPath(path) {
			return fmt.Errorf("%w: %q", ErrInvalidPatchPath, path)
		}
		if _, exists := paths[path]; exists {
			return fmt.Errorf("%w: duplicate path %q", ErrInvalidPatchPath, path)
		}
		paths[path] = struct{}{}
		return nil
	}
	for _, path := range patch.Unset {
		if err := add(path); err != nil {
			return err
		}
	}
	if len(patch.SetBSON) != 0 {
		raw := bson.Raw(patch.SetBSON)
		if err := raw.Validate(); err != nil {
			return fmt.Errorf("%w: invalid set BSON: %v", ErrInvalidPatch, err)
		}
		elements, err := raw.Elements()
		if err != nil {
			return fmt.Errorf("%w: invalid set BSON: %v", ErrInvalidPatch, err)
		}
		for _, element := range elements {
			if err := add(element.Key()); err != nil {
				return err
			}
		}
	}
	// Mongo 不接受同一次更新里的父子路径重叠。必须在 WAL 准入前拒绝，
	// 不能把确定无效的记录留给投影器无限重试或触发 fatal。
	for path := range paths {
		for i := range len(path) {
			if path[i] == '.' {
				if _, exists := paths[path[:i]]; exists {
					return fmt.Errorf("%w: overlapping paths %q and %q", ErrInvalidPatchPath, path[:i], path)
				}
			}
		}
	}
	return nil
}

func validPatchPath(path string) bool {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return false
	}
	for _, segment := range strings.Split(path, ".") {
		if segment == "" || strings.HasPrefix(segment, "$") {
			return false
		}
	}
	return true
}
