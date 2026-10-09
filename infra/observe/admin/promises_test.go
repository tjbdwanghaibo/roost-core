package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Admin commands are operator-facing entry points; a nameless definition or
// a nameless command would register or execute "nothing" without a word.
func TestAdminRegistriesRefuseNamelessAndHandlerlessCommands(t *testing.T) {
	reg := NewRegistry()
	err := reg.Register(CommandDef{Handler: func(context.Context, Command) (Result, error) { return Result{}, nil }})
	if !errors.Is(err, ErrCommandInvalid) || !strings.Contains(err.Error(), "name required") {
		t.Fatalf("nameless definition = %v", err)
	}
	err = reg.Register(CommandDef{Name: "reload"})
	if !errors.Is(err, ErrCommandInvalid) || !strings.Contains(err.Error(), "handler required for reload") {
		t.Fatalf("handlerless definition = %v", err)
	}
	if _, err := reg.Execute(context.Background(), Command{}); !errors.Is(err, ErrCommandInvalid) || !strings.Contains(err.Error(), "name required") {
		t.Fatalf("nameless command = %v", err)
	}
	meta := NewMetadataRegistry()
	if err := meta.Register(CommandMeta{}); !errors.Is(err, ErrCommandInvalid) || !strings.Contains(err.Error(), "metadata name required") {
		t.Fatalf("nameless metadata = %v", err)
	}
}
