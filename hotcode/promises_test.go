package hotcode

import (
	"errors"
	"strings"
	"testing"
)

// A patch point is addressed by name; an empty name, a non-function, or a
// second registration under the same name would make Replace ambiguous.
func TestRegistryRefusesNamelessNonFunctionAndDuplicatePoints(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("", func() {}); !errors.Is(err, ErrNameRequired) {
		t.Fatalf("empty name = %v", err)
	}
	if err := r.Register("point", 42); !errors.Is(err, ErrFuncRequired) {
		t.Fatalf("non-function = %v", err)
	}
	if err := r.Register("point", nil); !errors.Is(err, ErrFuncRequired) {
		t.Fatalf("nil function = %v", err)
	}
	if err := r.Register("point", func() {}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("point", func() {}); !errors.Is(err, ErrDuplicate) || !strings.Contains(err.Error(), "point") {
		t.Fatalf("duplicate = %v", err)
	}
	if err := r.Replace("", func() {}, Meta{}); !errors.Is(err, ErrNameRequired) {
		t.Fatalf("replace without name = %v", err)
	}
	if err := r.Replace("point", "not a func", Meta{}); !errors.Is(err, ErrFuncRequired) {
		t.Fatalf("replace with non-function = %v", err)
	}
}
