package servicerpc

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDir puts one file in a temp package directory.
func writeDir(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "svc.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const goodService = `package mail

import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error

//roost:rpc service_type=mail capability=service.mail
type Mail interface {
	// Send stores an envelope.
	Send(ctx context.Context, req SendRequest) (envelope Envelope, err error)
	//roost:rpc affinity=cursor
	List(ctx context.Context, playerID int64, cursor string, limit int) (page Page, err error)
	CancelClaim(ctx context.Context, playerID int64, mailID string, token string) (released bool, err error)
}
`

func TestParsesAnAnnotatedInterface(t *testing.T) {
	services, err := ParseDir(writeDir(t, goodService))
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 {
		t.Fatalf("found %d services, want 1", len(services))
	}
	service := services[0]
	if service.Interface != "Mail" || service.ServiceType != "mail" || service.Capability != "service.mail" {
		t.Fatalf("parsed %+v", service)
	}
	if len(service.Methods) != 3 {
		t.Fatalf("parsed %d methods, want 3", len(service.Methods))
	}
	// Declaration order is preserved, so the generated list reads like the
	// interface a person wrote.
	if service.Methods[0].Name != "Send" || service.Methods[2].Name != "CancelClaim" {
		t.Fatalf("methods came back in order %v", []string{
			service.Methods[0].Name, service.Methods[1].Name, service.Methods[2].Name,
		})
	}
	// ctx is dropped from the params; the rest keep their names, which is what
	// the wire field names come from.
	list := service.Methods[1]
	if len(list.Params) != 3 || list.Params[0].Name != "playerID" || list.Params[0].Type != "int64" {
		t.Fatalf("List params: %+v", list.Params)
	}
	if len(list.Results) != 1 || list.Results[0].Name != "page" || list.Results[0].Type != "Page" {
		t.Fatalf("List results: %+v", list.Results)
	}
	// The affinity key is the string parameter, not the int64 one: the key is
	// carried in the context and hashed, so it has to be a string. The first
	// version of this fixture used playerID and the rule that was added later
	// caught it.
	if list.Affinity != "cursor" {
		t.Fatalf("List affinity = %q", list.Affinity)
	}
	// The method's own comment travels; the marker does not — it is an
	// instruction to the generator, not documentation.
	if len(service.Methods[0].Doc) != 1 || !strings.Contains(service.Methods[0].Doc[0], "Send stores") {
		t.Fatalf("Send doc: %v", service.Methods[0].Doc)
	}
	if len(list.Doc) != 0 {
		t.Fatalf("the rpc marker leaked into the generated doc: %v", list.Doc)
	}
}

// An interface with no marker is not a service. The generator must not claim
// every interface in a package.
func TestAnUnmarkedInterfaceIsIgnored(t *testing.T) {
	services, err := ParseDir(writeDir(t, `package mail

import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error

type Store interface {
	Get(ctx context.Context, id string) (v string, err error)
}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 0 {
		t.Fatalf("an unmarked interface was picked up: %+v", services)
	}
}

// Each refusal names the method and the field, and says why. A generator that
// failed with "invalid input" would send the author reading the generator.
func TestRefusals(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   []string
	}{
		{
			name: "no service_type",
			source: `package m
import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error
//roost:rpc capability=service.m
type M interface { Do(ctx context.Context) (err error) }`,
			want: []string{"service_type"},
		},
		{
			name: "no capability",
			source: `package m
import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error
//roost:rpc service_type=m
type M interface { Do(ctx context.Context) (err error) }`,
			want: []string{"capability"},
		},
		{
			name: "unnamed result",
			source: `package m
import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error
//roost:rpc service_type=m capability=c
type M interface { Do(ctx context.Context) (Thing, error) }`,
			want: []string{"Do", "unnamed result", "Thing"},
		},
		{
			// Go's own grammar forbids MIXING named and unnamed parameters, so
			// `Do(ctx context.Context, int64)` never reaches this generator —
			// the parser rejects it first. The reachable shape is a fully
			// unnamed list, which is legal Go and gives the generator no field
			// names to work with.
			name: "unnamed parameters",
			source: `package m
import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error
//roost:rpc service_type=m capability=c
type M interface { Do(context.Context, int64) (v Thing, err error) }`,
			want: []string{"Do", "unnamed parameter", "int64"},
		},
		{
			name: "no error result",
			source: `package m
import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error
//roost:rpc service_type=m capability=c
type M interface { Do(ctx context.Context) (v Thing) }`,
			want: []string{"Do", "does not end in error"},
		},
		{
			name: "no context",
			source: `package m

var ErrRequestInvalid error
//roost:rpc service_type=m capability=c
type M interface { Do(id int64) (err error) }`,
			want: []string{"Do", "context.Context"},
		},
		{
			name: "duration on the wire",
			source: `package m
import (
	"context"
	"time"
)

var ErrRequestInvalid error
//roost:rpc service_type=m capability=c
type M interface { Do(ctx context.Context, wait time.Duration) (err error) }`,
			want: []string{"Do", "wait", "time.Duration", "expiresInSeconds"},
		},
		{
			name: "time on the wire",
			source: `package m
import (
	"context"
	"time"
)

var ErrRequestInvalid error
//roost:rpc service_type=m capability=c
type M interface { Do(ctx context.Context, at time.Time) (err error) }`,
			want: []string{"at", "time.Time", "createdAtUnix"},
		},
		{
			name: "any on the wire",
			source: `package m
import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error
//roost:rpc service_type=m capability=c
type M interface { Do(ctx context.Context, payload any) (err error) }`,
			want: []string{"payload", "typed client"},
		},
		{
			name: "unexported type",
			source: `package m
import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error
//roost:rpc service_type=m capability=c
type M interface { Do(ctx context.Context, state queueState) (err error) }`,
			want: []string{"state", "queueState", "another package cannot construct"},
		},
		{
			name: "embedded interface",
			source: `package m
import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error
//roost:rpc service_type=m capability=c
type M interface {
	Base
	Do(ctx context.Context) (err error)
}`,
			want: []string{"embeds an interface"},
		},
		{
			name: "affinity names no parameter",
			source: `package m
import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error
//roost:rpc service_type=m capability=c
type M interface {
	//roost:rpc affinity=queueKey
	Do(ctx context.Context, id int64) (err error)
}`,
			want: []string{"Do", "affinity=queueKey", "no parameter queueKey"},
		},
		{
			name: "no methods",
			source: `package m

var ErrRequestInvalid error
//roost:rpc service_type=m capability=c
type M interface{}`,
			want: []string{"no methods"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDir(writeDir(t, tc.source))
			if err == nil {
				t.Fatal("the generator accepted it")
			}
			for _, fragment := range tc.want {
				if !strings.Contains(err.Error(), fragment) {
					t.Fatalf("the error does not mention %q: %v", fragment, err)
				}
			}
		})
	}
}

// Parsing is deterministic across runs: a generator whose output depends on
// map iteration order makes every regeneration a diff.
func TestParsingIsDeterministic(t *testing.T) {
	dir := writeDir(t, goodService)
	first, err := ParseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 8; run++ {
		again, err := ParseDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(first) {
			t.Fatalf("run %d found %d services, first run found %d", run, len(again), len(first))
		}
		for index := range again {
			if again[index].Interface != first[index].Interface {
				t.Fatalf("run %d ordered interfaces differently", run)
			}
			for m := range again[index].Methods {
				if again[index].Methods[m].Name != first[index].Methods[m].Name {
					t.Fatalf("run %d ordered methods differently", run)
				}
			}
		}
	}
}

// --- the checks that only running against real interfaces produced ---

// A wire-unsafe type inside a struct that is passed WHOLE must be refused,
// and the error must name the field path rather than the parameter.
//
// This is the mistake the first version of the generator missed, and it is the
// one that actually happened: mail's SendRequest carries an
// ExpiresIn time.Duration, so `Send(ctx, req SendRequest)` passed a
// name-only check while putting a nanosecond count on the wire. Checking the
// parameter's own type is checking the part that is easy to get right.
func TestAnUnsafeTypeInsideAPassedStructIsRefused(t *testing.T) {
	_, err := ParseDir(writeDir(t, `package mail

import (
	"context"
	"time"
)

var ErrRequestInvalid error

type SendRequest struct {
	Subject   string
	ExpiresIn time.Duration
}

//roost:rpc service_type=mail capability=service.mail
type Mail interface {
	Send(ctx context.Context, req SendRequest) (envelope Envelope, err error)
}
`))
	if err == nil {
		t.Fatal("a time.Duration inside a passed struct was accepted")
	}
	// The path, not just the parameter: an author reading "req is unsafe" has
	// to go looking; "req.ExpiresIn" is the answer.
	if !strings.Contains(err.Error(), "req.ExpiresIn") {
		t.Fatalf("the error does not name the field path: %v", err)
	}
	if !strings.Contains(err.Error(), "expiresInSeconds") {
		t.Fatalf("the error does not say what to do instead: %v", err)
	}
}

// A struct whose meaning lives in an unexported field must be refused.
//
// chat's SystemToken is the case: its authority is an unexported bool that
// only GrantSystem can set, so a codec marshals it to {} and the peer receives
// a token that grants nothing. That happens to fail closed, which is luck
// rather than design — one exported field added to such a type turns the luck
// into a hole, silently.
func TestAStructWithUnexportedFieldsIsRefused(t *testing.T) {
	_, err := ParseDir(writeDir(t, `package chat

import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error

type SystemToken struct{ granted bool }

//roost:rpc service_type=chat capability=service.chat
type Chat interface {
	PublishSystem(ctx context.Context, token SystemToken, req Request) (message Message, err error)
}
`))
	if err == nil {
		t.Fatal("a type whose authority is an unexported field was accepted onto the wire")
	}
	if !strings.Contains(err.Error(), "token.granted") {
		t.Fatalf("the error does not name the dropped field: %v", err)
	}
	if !strings.Contains(err.Error(), "does not arrive") {
		t.Fatalf("the error does not say what goes wrong: %v", err)
	}
}

// A struct with no exported fields at all marshals to {} and arrives as a zero
// value. Refused for the same reason, reported differently because the author's
// fix is different: there is nothing to un-unexport.
func TestAStructWithNoExportedFieldsIsRefused(t *testing.T) {
	_, err := ParseDir(writeDir(t, `package m

import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error

type Opaque struct{}

//roost:rpc service_type=m capability=c
type M interface {
	Do(ctx context.Context, o Opaque) (err error)
}
`))
	if err == nil {
		t.Fatal("a struct with no exported fields was accepted")
	}
	if !strings.Contains(err.Error(), "marshals to {}") {
		t.Fatalf("the error does not explain: %v", err)
	}
}

// Legitimate nesting — a struct of structs and a map of structs — must pass.
// A checker that refused everything would be as useless as one that refused
// nothing, and easier to mistake for working.
func TestLegitimateNestingIsAccepted(t *testing.T) {
	services, err := ParseDir(writeDir(t, `package m

import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error

type Inner struct{ AtUnix int64 }
type Outer struct {
	Name  string
	Inner Inner
	Tags  map[string]Inner
	List  []Inner
}
type Res struct{ OK bool }

//roost:rpc service_type=m capability=c
type M interface {
	Do(ctx context.Context, req Outer) (res Res, err error)
}
`))
	if err != nil {
		t.Fatalf("legitimate nesting was refused: %v", err)
	}
	if len(services) != 1 || len(services[0].Methods) != 1 {
		t.Fatalf("parsed %+v", services)
	}
}

// A type declared in another package cannot be resolved, so it is checked by
// name only. That limit is stated rather than hidden: the name check still
// catches time.Duration and time.Time, which are the foreign types that
// actually turn up.
func TestAForeignTypeIsCheckedByNameOnly(t *testing.T) {
	// time.Duration is foreign and caught.
	if _, err := ParseDir(writeDir(t, `package m

import (
	"context"
	"time"
)

var ErrRequestInvalid error

//roost:rpc service_type=m capability=c
type M interface {
	Do(ctx context.Context, wait time.Duration) (err error)
}
`)); err == nil {
		t.Fatal("a foreign time.Duration was accepted")
	}
	// A foreign struct is accepted, because its fields are not visible here.
	if _, err := ParseDir(writeDir(t, `package m

import (
	"context"
	"net/url"
)

var ErrRequestInvalid error

//roost:rpc service_type=m capability=c
type M interface {
	Do(ctx context.Context, u url.Values) (err error)
}
`)); err != nil {
		t.Fatalf("a foreign type was refused although its fields are not visible: %v", err)
	}
}

// A self-referential type must not hang the generator.
func TestARecursiveTypeIsRefusedRatherThanLoopingForever(t *testing.T) {
	_, err := ParseDir(writeDir(t, `package m

import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error

type Node struct {
	Name  string
	Child *Node
}

//roost:rpc service_type=m capability=c
type M interface {
	Do(ctx context.Context, n Node) (err error)
}
`))
	if err == nil {
		t.Fatal("a recursive type was accepted; a payload of unbounded depth is not a payload")
	}
	if !strings.Contains(err.Error(), "8 levels") {
		t.Fatalf("the error does not explain the bound: %v", err)
	}
}

// The derived affinity form exists for the case that actually occurs: a key
// computed from a value rather than passed as one.
//
// match keeps a whole queue in one key and is identified by a Queue struct
// with a Key method, so a bare string parameter would have meant changing the
// service's API to satisfy the generator — which is the wrong direction.
func TestADerivedAffinityKeyIsAccepted(t *testing.T) {
	services, err := ParseDir(writeDir(t, `package match

import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error

type Queue struct{ Mode string }

func (q Queue) Key() string { return q.Mode }

//roost:rpc service_type=match capability=service.match
type Match interface {
	//roost:rpc affinity=queue.Key()
	Length(ctx context.Context, queue Queue) (length int, err error)
}
`))
	if err != nil {
		t.Fatalf("a derived affinity key was refused: %v", err)
	}
	if got := services[0].Methods[0].Affinity; got != "queue.Key()" {
		t.Fatalf("parsed affinity %q", got)
	}
}

// A derived form that names no parameter is refused, and the error names the
// parameter it looked for rather than the whole expression.
func TestADerivedAffinityKeyMustNameAParameter(t *testing.T) {
	_, err := ParseDir(writeDir(t, `package m

import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error

//roost:rpc service_type=m capability=c
type M interface {
	//roost:rpc affinity=shelf.Key()
	Do(ctx context.Context, queue Queue) (err error)
}
`))
	if err == nil {
		t.Fatal("an affinity expression naming no parameter was accepted")
	}
	if !contains(err.Error(), "no parameter shelf") {
		t.Fatalf("the error does not name the missing parameter: %v", err)
	}
}

// A non-string bare key is refused, and the error suggests the derived form
// rather than only saying no.
func TestANonStringBareKeySuggestsTheDerivedForm(t *testing.T) {
	_, err := ParseDir(writeDir(t, `package m

import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error

//roost:rpc service_type=m capability=c
type M interface {
	//roost:rpc affinity=queue
	Do(ctx context.Context, queue Queue) (err error)
}
`))
	if err == nil {
		t.Fatal("a struct as a bare affinity key was accepted")
	}
	if !contains(err.Error(), "affinity=queue.Key()") {
		t.Fatalf("the error does not suggest the derived form: %v", err)
	}
}

// A package with no ErrRequestInvalid is refused before anything is
// generated.
//
// The generated transport answers an undecodable payload with it, so the
// package has to have one. Checked here rather than left to the compiler
// because the compiler's answer — "undefined: ErrRequestInvalid" inside a
// generated file — sends the reader into generated code to find out what wants
// it. The second service this generator ran against was missing it.
func TestAPackageWithoutErrRequestInvalidIsRefused(t *testing.T) {
	_, err := ParseDir(writeDir(t, `package m

import "context"

//roost:rpc service_type=m capability=c
type M interface {
	Do(ctx context.Context, playerID int64) (err error)
}
`))
	if err == nil {
		t.Fatal("a package with no ErrRequestInvalid was accepted; the generated transport " +
			"would not compile and the error would point into generated code")
	}
	for _, fragment := range []string{"ErrRequestInvalid", "cannot decode", "its own segment"} {
		if !contains(err.Error(), fragment) {
			t.Fatalf("the error does not mention %q: %v", fragment, err)
		}
	}
}

// A package that already declares a name the generated file emits is refused
// with that name in the message.
//
// The confirmed case: account declared `type Server struct` for a row in its
// game-server list. Generating a transport whose process host is also called
// Server produced a file that did not compile, and the compiler's answer —
// "Server redeclared in this block", pointing at the generated file — says
// where the duplicate is and nothing about why it is there or which side can
// move. The generated side cannot.
func TestANameThePackageAlreadyDeclaresIsRefused(t *testing.T) {
	// Each case collides on a different KIND of declaration, because a name
	// taken by a function or a constant is the dangerous one: it can change
	// what the rest of the package means before anything fails to build.
	cases := map[string]struct {
		extra    string
		collides string
	}{
		"a type":     {extra: "type Server struct{ ID int32 }", collides: "Server"},
		"a function": {extra: "func Capability() string { return \"\" }", collides: "Capability"},
		"a constant": {extra: "const ServiceType = \"legacy\"", collides: "ServiceType"},
		"a variable": {extra: "var Methods []string", collides: "Methods"},
		// Unexported names collide too: the wrapper type is package-private
		// and still occupies the identifier.
		"an unexported type": {extra: "type capability struct{}", collides: "capability"},
		// A per-method name, which is derived rather than fixed.
		"a method constant": {extra: "const MethodDo = \"legacy\"", collides: "MethodDo"},
		"a wire type":       {extra: "type rpcDoRequest struct{ X int }", collides: "rpcDoRequest"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseDir(writeDir(t, `package m

import "context"

var ErrRequestInvalid error

`+tc.extra+`

//roost:rpc service_type=m capability=c
type M interface {
	Do(ctx context.Context, playerID int64) (err error)
}
`))
			if err == nil {
				t.Fatalf("a package already declaring %s was accepted; the generated file "+
					"would not compile", tc.collides)
			}
			if !contains(err.Error(), tc.collides) {
				t.Fatalf("the error does not name the colliding identifier %q: %v", tc.collides, err)
			}
			if !contains(err.Error(), "rename the existing one") {
				t.Fatalf("the error does not say which side has to move: %v", err)
			}
		})
	}
}

// A package that declares none of them is still accepted — the rule must not
// refuse every package by matching too broadly.
func TestANamePackageDeclarationsThatDoNotCollideAreAccepted(t *testing.T) {
	services, err := ParseDir(writeDir(t, `package m

import "context"

var ErrRequestInvalid error

// Names that are CLOSE to generated ones and must not trip the rule.
type GameServer struct{ ID int32 }
type ServerStatus string

const CapabilityNamePrefix = "service."

func Capabilities() []string { return nil }

//roost:rpc service_type=m capability=c
type M interface {
	Do(ctx context.Context, playerID int64) (err error)
}
`))
	if err != nil {
		t.Fatalf("a package with merely similar names was refused: %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("parsed %d services, want 1", len(services))
	}
}

// Two marked interfaces in one package are refused.
//
// The generated file declares Server, CapabilityName, BusClient and a dozen
// more names at package scope, and those names are fixed so that a caller
// reads pkg.Server the same way in every service. Two transports in one
// package declare each of them twice.
//
// The case that raised it: the global package held both a routing Service and
// an ActivityService — two Mods, two capabilities, and app.Service is one per
// process, so they were always two deployments sharing one package.
func TestTwoMarkedInterfacesInOnePackageAreRefused(t *testing.T) {
	_, err := ParseDir(writeDir(t, `package m

import "context"

var ErrRequestInvalid error

//roost:rpc service_type=routing capability=service.routing
type Routing interface {
	Resolve(ctx context.Context, gameSID int32) (bound bool, err error)
}

//roost:rpc service_type=activity capability=service.activity
type Activities interface {
	Open(ctx context.Context, key string) (opened bool, err error)
}
`))
	if err == nil {
		t.Fatal("two marked interfaces in one package were accepted; the two generated files " +
			"would each declare Server and CapabilityName and would not compile together")
	}
	for _, fragment := range []string{"Routing", "Activities", "separate packages"} {
		if !contains(err.Error(), fragment) {
			t.Fatalf("the error does not mention %q: %v", fragment, err)
		}
	}
}

// And one marked interface beside unmarked ones is still fine — the rule must
// count MARKED interfaces, not interfaces.
func TestOneMarkedInterfaceBesideUnmarkedOnesIsAccepted(t *testing.T) {
	services, err := ParseDir(writeDir(t, `package m

import "context"

var ErrRequestInvalid error

// Collaborator seams, not services. These must not count.
type Verifier interface {
	Verify(ctx context.Context, token string) error
}

type Store interface {
	Get(ctx context.Context, id string) (string, error)
}

//roost:rpc service_type=m capability=c
type M interface {
	Do(ctx context.Context, playerID int64) (err error)
}
`))
	if err != nil {
		t.Fatalf("one marked interface beside unmarked ones was refused: %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("parsed %d services, want 1", len(services))
	}
}

// -check compares the generated output against what is on disk and fails on
// any difference.
//
// This mode used to return right after the refusals ran, so it exited 0 on a
// generated file that was stale, hand-edited, or produced by a different
// version of this generator — while being documented in two READMEs as the CI
// drift gate. A gate that cannot fail is worse than no gate: it reports that
// the committed transport matches the interface when nobody looked.
//
// It was found by deliberately appending a comment to a committed generated
// file and watching -check exit 0.
func TestCheckDetectsDrift(t *testing.T) {
	dir := writeDir(t, goodService)

	// Nothing generated yet: missing must be reported, and reported AS
	// missing, because "nobody ran the generator" and "someone edited the
	// output" are different problems with different fixes.
	err := Run([]string{"-dir", dir, "-check"}, io.Discard)
	if err == nil {
		t.Fatal("-check passed with no generated file at all")
	}
	if !contains(err.Error(), "missing") {
		t.Fatalf("the error does not distinguish a missing file: %v", err)
	}

	// Generate, then -check must pass.
	if err := Run([]string{"-dir", dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-dir", dir, "-check"}, io.Discard); err != nil {
		t.Fatalf("-check failed immediately after generating: %v", err)
	}

	// Drift the file the way a hand edit or an older generator would.
	path := filepath.Join(dir, "mail_rpc_gen.go")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(content, []byte("\n// hand edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	err = Run([]string{"-dir", dir, "-check"}, io.Discard)
	if err == nil {
		t.Fatal("-check passed on a generated file that does not match the interface")
	}
	if !contains(err.Error(), "mail_rpc_gen.go") {
		t.Fatalf("the error does not name the stale file: %v", err)
	}
	if contains(err.Error(), "missing") {
		t.Fatalf("a differing file was reported as missing: %v", err)
	}

	// And -check must not have repaired it: writing in check mode would make
	// the gate pass on its second run and hide the drift from CI.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) == len(content) {
		t.Fatal("-check rewrote the file; a check mode that writes cannot gate anything")
	}
}

// The refusals still run before the comparison, so an author fixing an
// interface gets the design answer rather than a diff.
func TestCheckReportsRefusalsBeforeDrift(t *testing.T) {
	// No generated file AND an unsafe type: the refusal must win.
	err := Run([]string{"-dir", writeDir(t, `package m

import (
	"context"
	"time"
)

var ErrRequestInvalid error

//roost:rpc service_type=m capability=c
type M interface {
	Do(ctx context.Context, wait time.Duration) (err error)
}
`), "-check"}, io.Discard)
	if err == nil {
		t.Fatal("-check accepted a time.Duration on the wire")
	}
	if contains(err.Error(), "missing") || contains(err.Error(), "does not match") {
		t.Fatalf("the drift report shadowed the refusal, so the author sees a diff instead of "+
			"the design problem: %v", err)
	}
}
