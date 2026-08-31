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
	ErrMixedMutationForms  = errors.New("dataengine: mixed canonical and legacy mutation fields")
)

func hasLegacyFields(m Mutation) bool {
	return m.EntityID != 0 || m.Database != "" || m.DatabaseScope != 0 || m.Resource != "" || m.Version != 0
}

func hasCanonicalFields(m Mutation) bool {
	return m.Key != (DocumentKey{}) || m.Kind != 0 || m.ExpectedVersion != 0 || m.NextVersion != 0 || !m.Patch.Empty()
}

// CanonicalizeMutation upgrades one v1 full after-image to the canonical
// mutation representation. Mixed forms are rejected so identity/version
// conflicts cannot be silently resolved.
func CanonicalizeMutation(m Mutation) (Mutation, error) {
	m = CloneMutation(m)
	legacy := hasLegacyFields(m)
	if legacy && hasCanonicalFields(m) {
		return Mutation{}, ErrMixedMutationForms
	}
	if legacy {
		m.Key = DocumentKey{
			Database: m.Database,
			Scope:    DatabaseScope(m.DatabaseScope),
			Resource: m.Resource,
			ID:       m.EntityID,
		}
		m.Kind = MutationPut
		if m.Remote != nil && m.Remote.Delete {
			m.Kind = MutationDelete
		}
		m.NextVersion = m.Version
		if m.NextVersion > 0 {
			m.ExpectedVersion = m.NextVersion - 1
		}
		m.EntityID = 0
		m.Database = ""
		m.DatabaseScope = 0
		m.Resource = ""
		m.Version = 0
	}
	if err := ValidateMutation(m); err != nil {
		return Mutation{}, err
	}
	return m, nil
}

func ValidateMutation(m Mutation) error {
	if hasLegacyFields(m) {
		return ErrMixedMutationForms
	}
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
	for _, path := range patch.Unset {
		if !validPatchPath(path) {
			return fmt.Errorf("%w: %q", ErrInvalidPatchPath, path)
		}
	}
	if len(patch.SetBSON) == 0 {
		return nil
	}
	raw := bson.Raw(patch.SetBSON)
	if err := raw.Validate(); err != nil {
		return fmt.Errorf("%w: invalid set BSON: %v", ErrInvalidPatch, err)
	}
	elements, err := raw.Elements()
	if err != nil {
		return fmt.Errorf("%w: invalid set BSON: %v", ErrInvalidPatch, err)
	}
	for _, element := range elements {
		if path := element.Key(); !validPatchPath(path) {
			return fmt.Errorf("%w: %q", ErrInvalidPatchPath, path)
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
