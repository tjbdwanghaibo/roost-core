package servicerpc

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-codegen/internal/genutil"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// The generated transport is locked to a golden file, so a template change is
// a reviewable diff rather than something that turns up in a consumer's
// package.
//
// The definition below is shaped after the interface this generator was built
// against: a method that passes a whole request struct, methods that take a
// caller identity as a parameter, a method whose result is a bare bool, and
// one carrying an affinity marker. Anything the template gets wrong for those
// shapes it gets wrong for every service.
func TestGoldenTransport(t *testing.T) {
	services, err := ParseDir(writeDir(t, goldenService))
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 {
		t.Fatalf("parsed %d services, want 1", len(services))
	}
	files, err := Generate(services[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "shop_rpc_gen.go" || files[1].Name != "shop_rpc_assembly_gen.go" {
		t.Fatalf("generated files = %v, want the transport half then the assembly half", fileNames(files))
	}
	for _, file := range files {
		genutil.AssertGolden(t, filepath.Join("testdata", "golden", file.Name+".txt"), file.Content, *updateGolden)
	}
}

// Generation is deterministic: the same input produces byte-identical output.
// A generator whose output shifts between runs makes every regeneration a
// diff, and then nobody reads the diffs.
func TestGenerationIsDeterministic(t *testing.T) {
	services, err := ParseDir(writeDir(t, goldenService))
	if err != nil {
		t.Fatal(err)
	}
	first := generateJoined(t, services[0])
	for run := 0; run < 8; run++ {
		again := generateJoined(t, services[0])
		if string(again) != string(first) {
			t.Fatalf("run %d produced different output", run)
		}
	}
}

// The generated source parses and formats as Go. Generate already runs
// go/format, so a template that produced something unparseable fails there —
// this asserts that it is checked rather than that the output happens to look
// right.
func TestGeneratedSourceIsValidGo(t *testing.T) {
	services, err := ParseDir(writeDir(t, goldenService))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(services[0]); err != nil {
		t.Fatalf("the generated source does not format as Go: %v", err)
	}
}

// A wire type must not be exported.
//
// That is the design claim the generator makes and it has to hold in the
// output: a caller outside the package cannot construct a wire struct, so it
// cannot route around the client method that takes the caller's identity as a
// parameter. The first version exported them and collided with a hand-written
// type of the same name, which is how the claim got examined at all.
func TestWireTypesAreUnexported(t *testing.T) {
	services, err := ParseDir(writeDir(t, goldenService))
	if err != nil {
		t.Fatal(err)
	}
	content := generateJoined(t, services[0])
	source := string(content)
	for _, exported := range []string{
		"type BuyRequest struct", "type BuyResponse struct",
		"type ResponseStatus struct",
	} {
		if contains(source, exported) {
			t.Fatalf("the generated source declares %q; a wire type that a caller can construct "+
				"is a way around the client method that asks for the caller's identity", exported)
		}
	}
	for _, unexported := range []string{
		"type rpcBuyRequest struct", "type rpcBuyResponse struct", "type rpcStatus struct",
	} {
		if !contains(source, unexported) {
			t.Fatalf("the generated source is missing %q", unexported)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// goldenService covers the shapes that matter: a whole-struct parameter, a
// caller identity as a parameter, a bare-bool result, an affinity marker, and
// a method with no results at all.
const goldenService = `package shop

import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error

type BuyRequest struct {
	ProductID   string
	AmountMinor int64
}

type Receipt struct {
	OrderID string
}

type Page struct {
	Items []string
}

//roost:rpc service_type=shop capability=service.shop
type Shop interface {
	// Buy places an order. Idempotent per RequestID.
	Buy(ctx context.Context, playerID int64, req BuyRequest) (receipt Receipt, err error)

	//roost:rpc affinity=shelfID
	Browse(ctx context.Context, shelfID string, limit int) (page Page, err error)

	// Cancel reports whether it cancelled anything.
	Cancel(ctx context.Context, playerID int64, orderID string) (cancelled bool, err error)

	// Touch has no results beyond the error.
	Touch(ctx context.Context, playerID int64) (err error)
}
`

// An affinity marker must change the generated code, in both places it has to.
//
// It was parsed and ignored in the first version: the marker validated, a
// context key was never set, and the picker was never installed — so a service
// that declared affinity got round-robin routing and read as configured. A
// marker that does nothing is worse than no marker.
func TestAnAffinityMarkerReachesTheGeneratedClient(t *testing.T) {
	services, err := ParseDir(writeDir(t, goldenService))
	if err != nil {
		t.Fatal(err)
	}
	content := generateJoined(t, services[0])
	source := string(content)
	// The method attaches the key.
	if !contains(source, "servicerpc.WithAffinityKey(ctx, shelfID)") {
		t.Fatal("the affinity method does not attach its key to the context; the call would be " +
			"routed round-robin and the marker would mean nothing")
	}
	// And the constructor installs the picker that reads it. Without this the
	// key travels and nothing looks at it.
	if !contains(source, "servicerpc.WithKeyAffinity(servicerpc.AffinityKeyFromContext)") {
		t.Fatal("the constructor does not install the affinity picker; the key would be set and " +
			"never read")
	}
	// A method without the marker must not attach anything.
	buyStart := indexOf(source, "func (c *BusClient) Buy(")
	buyEnd := indexOf(source[buyStart:], "\n}\n")
	if buyStart < 0 || buyEnd < 0 {
		t.Fatal("could not find the Buy client method")
	}
	if contains(source[buyStart:buyStart+buyEnd], "WithAffinityKey") {
		t.Fatal("a method with no affinity marker attaches an affinity key")
	}
}

// A service with no affinity anywhere must not install the picker: an option
// nothing needs is an option someone will wonder about.
func TestNoAffinityMeansNoPickerOption(t *testing.T) {
	services, err := ParseDir(writeDir(t, `package plain

import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error

//roost:rpc service_type=plain capability=service.plain
type Plain interface {
	Do(ctx context.Context, playerID int64) (err error)
}
`))
	if err != nil {
		t.Fatal(err)
	}
	content := generateJoined(t, services[0])
	if contains(string(content), "WithKeyAffinity") {
		t.Fatal("the picker option was installed for a service that routes by nothing")
	}
}

// An affinity key has to be a string: it is carried in the context and hashed
// to pick an instance. Refusing a non-string names the parameter and says what
// to do, rather than generating code that will not compile.
func TestANonStringAffinityKeyIsRefused(t *testing.T) {
	_, err := ParseDir(writeDir(t, `package m

import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error

//roost:rpc service_type=m capability=c
type M interface {
	//roost:rpc affinity=shardID
	Do(ctx context.Context, shardID int32) (err error)
}
`))
	if err == nil {
		t.Fatal("a non-string affinity key was accepted")
	}
	for _, fragment := range []string{"Do", "affinity=shardID", "shardID is int32", "has to be a string"} {
		if !contains(err.Error(), fragment) {
			t.Fatalf("the error does not mention %q: %v", fragment, err)
		}
	}
}

// An interface with a method of its own name must still produce a working
// capability wrapper.
//
// This is a regression test for a bug the second service found: the wrapper
// embedded the interface, and Go names an embedded field after its type — so
// `type capability struct{ Rank }` gave the struct a FIELD called Rank that
// shadowed the interface's METHOD called Rank, and the wrapper did not satisfy
// the interface at all. It compiled for mail (no method called Mail) and
// failed for rank.
//
// The fix is explicit forwarding, and this pins it: the shape is legal Go and
// a generator that only ever saw the first service would not have met it.
func TestAnInterfaceWithAMethodOfItsOwnNameGeneratesAWorkingWrapper(t *testing.T) {
	services, err := ParseDir(writeDir(t, `package rank

import "context"

// Every service package must declare this: the generated transport reports it
// for a payload it cannot decode. The generator checks for it by name.
var ErrRequestInvalid error

type Board struct{ ID string }
type Entry struct{ Rank int64 }

//roost:rpc service_type=rank capability=service.rank
type Rank interface {
	// Rank has the same name as the interface, which is legal and awkward
	// rather than wrong.
	Rank(ctx context.Context, board Board, ownerID int64) (entry Entry, found bool, err error)
	Size(ctx context.Context, board Board) (size int64, err error)
}
`))
	if err != nil {
		t.Fatal(err)
	}
	content := generateJoined(t, services[0])
	source := string(content)
	// The wrapper holds an unexported field and forwards, rather than
	// embedding — embedding is what created the shadowing.
	if !contains(source, "type capability struct{ inner Rank }") {
		t.Fatal("the wrapper embeds the interface; a method of the interface's own name would " +
			"be shadowed by the embedded field and the wrapper would not satisfy the interface")
	}
	if !contains(source, "func (c capability) Rank(ctx context.Context, board Board, ownerID int64) (Entry, bool, error)") {
		t.Fatalf("the wrapper does not forward Rank explicitly:\n%s", source)
	}
	if !contains(source, "return c.inner.Rank(ctx, board, ownerID)") {
		t.Fatal("the forwarder does not call through to the wrapped service")
	}
	// And the compile-time assertion the generated file carries must be there,
	// because that is what would have caught the original bug at generate
	// time rather than at the consumer's build.
	if !contains(source, "_ Rank = capability{}") {
		t.Fatal("the generated file does not assert that the wrapper satisfies the interface")
	}
}

// Every package-scope name the generated file declares is listed in
// emittedNames.
//
// emittedNames is a hand-written list, and a hand-written list beside a
// template is a second place to forget. Forgetting here is quiet in the worst
// way: the collision rule keeps accepting a package that declares the missed
// name, and the failure surfaces as "redeclared in this block" inside
// generated code — exactly the error the rule exists to prevent.
//
// So the list is checked against the OUTPUT rather than against the template
// text: parse the generated source, collect what it declares at package
// scope, and require the list to cover it. That direction is the load-bearing
// one — a name emitted but unlisted is the bug.
func TestEveryEmittedNameIsListed(t *testing.T) {
	services, err := ParseDir(writeDir(t, goldenService))
	if err != nil {
		t.Fatal(err)
	}
	parsed := parseGenerated(t, services[0])
	listed := map[string]bool{}
	for _, name := range emittedNames(services[0]) {
		listed[name] = true
	}
	for name := range declaredNames(parsed) {
		if !listed[name] {
			t.Fatalf("the generated file declares %q at package scope but emittedNames does not "+
				"list it, so a package that already declares %q would be accepted and then fail "+
				"to compile. Add it to emittedNames in validate.go", name, name)
		}
	}
}

// And nothing is listed that the generated file does not declare.
//
// A stale entry is a lesser fault than a missing one — it refuses a package
// for a name the transport no longer uses — but it is still a refusal nobody
// can act on, because renaming the existing declaration would fix nothing
// visible.
func TestNothingIsListedThatIsNotEmitted(t *testing.T) {
	services, err := ParseDir(writeDir(t, goldenService))
	if err != nil {
		t.Fatal(err)
	}
	parsed := parseGenerated(t, services[0])
	declared := declaredNames(parsed)
	for _, name := range emittedNames(services[0]) {
		if _, ok := declared[name]; !ok {
			t.Fatalf("emittedNames lists %q, but the generated file does not declare it; the "+
				"entry is stale and refuses packages for no reason", name)
		}
	}
}

// A ClientMod's dependency must name a Mod, because app resolves dependencies
// by Mod name. mods.ModBus is a CAPABILITY name that no Mod is called, so a
// client depending on it fails assembly with `unknown mod dependency "bus"` in
// every real process — which every generated client did until a generated
// game template was started (U-0024). The bus is published by the NATS Mod.
func TestTheGeneratedClientDependsOnTheModThatPublishesTheBus(t *testing.T) {
	services, err := ParseDir(writeDir(t, goldenService))
	if err != nil {
		t.Fatal(err)
	}
	content := generateJoined(t, services[0])
	if !strings.Contains(string(content), "func (m *ClientMod) DependsOn() []app.ModName { return []app.ModName{mods.ModNats} }") {
		t.Fatalf("ClientMod.DependsOn does not name the NATS mod:\n%s", content)
	}
	if strings.Contains(string(content), "DependsOn() []app.ModName { return []app.ModName{mods.ModBus} }") {
		t.Fatal("ClientMod depends on the bus capability name, which no Mod is called")
	}
}

// generateJoined renders both halves and joins them, for assertions about what
// the generated transport as a whole says. TestGoldenTransport pins each file.
func generateJoined(t *testing.T, service Service) []byte {
	t.Helper()
	files, err := Generate(service)
	if err != nil {
		t.Fatal(err)
	}
	var joined []byte
	for _, file := range files {
		joined = append(joined, file.Content...)
	}
	return joined
}

// parseGenerated parses each generated file on its own: the two halves are
// separate files of one package, so the declared names are their union.
func parseGenerated(t *testing.T, service Service) map[string]*ast.File {
	t.Helper()
	files, err := Generate(service)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	parsed := make(map[string]*ast.File, len(files))
	for _, file := range files {
		parsedFile, err := parser.ParseFile(fset, file.Name, file.Content, parser.ParseComments)
		if err != nil {
			t.Fatalf("%s: %v", file.Name, err)
		}
		parsed[file.Name] = parsedFile
	}
	return parsed
}

func fileNames(files []File) []string {
	names := make([]string, 0, len(files))
	for _, file := range files {
		names = append(names, file.Name)
	}
	return names
}
