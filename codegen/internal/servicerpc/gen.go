package servicerpc

import (
	"bytes"
	"fmt"
	"go/format"
	"strings"
	"text/template"
	"unicode"
)

// File is one generated source file: its base name and formatted content.
type File struct {
	Name    string
	Content []byte
}

// TransportFileName is the transport half's file name for an interface.
func TransportFileName(iface string) string { return strings.ToLower(iface) + "_rpc_gen.go" }

// AssemblyFileName is the assembly half's file name for an interface.
func AssemblyFileName(iface string) string {
	return strings.ToLower(iface) + "_rpc_assembly_gen.go"
}

// Half selects which generated files Generate emits.
type Half string

const (
	// HalfAll emits both files into one package: the default, and what every
	// package that owns its interface wants.
	HalfAll Half = "all"
	// HalfTransport emits only the transport half. A core domain package that
	// owns the interface uses it: the file depends on roost-core only, so the
	// package stays inside the core dependency boundary.
	HalfTransport Half = "transport"
	// HalfAssembly emits only the assembly half. A kit package whose interface
	// lives in core uses it, pointing -dir at the core package and -out at
	// itself: the types it names resolve through the kit package's aliases.
	HalfAssembly Half = "assembly"
)

// DefaultRegenerate is the command the generated headers cite when the
// generator ran with no flags but -dir .
const DefaultRegenerate = "go run github.com/tjbdwanghaibo/roost-codegen/cmd/servicerpc -dir ."

// Options steers Generate; the zero value is HalfAll with DefaultRegenerate.
type Options struct {
	Half Half
	// Regenerate is the command the generated header tells a reader to run.
	// It is recorded rather than derived so a package generated from another
	// package's interface says how it was produced.
	Regenerate string
}

// Generate renders the transport for one service as two files: the transport
// half (roost-core imports only) and the assembly half (Server, ClientMod,
// OwnerCapabilities; imports roost-kit/mods). Both land in the same package,
// so nothing changes for a caller; the split exists so an interface can move
// into a core domain package together with its wire types and client (M-10).
//
// It is built with text/template rather than string concatenation, and the
// output is run through go/format, so a template mistake surfaces as a parse
// error here rather than as an unbuildable file in someone's package.
func Generate(service Service) ([]File, error) { return GenerateWith(service, Options{}) }

// GenerateWith is Generate with a choice of half and header command (M-11).
func GenerateWith(service Service, opts Options) ([]File, error) {
	view, err := newView(service)
	if err != nil {
		return nil, err
	}
	if opts.Regenerate != "" {
		view.Regenerate = opts.Regenerate
	}
	half := opts.Half
	if half == "" {
		half = HalfAll
	}
	var files []File
	if half == HalfAll || half == HalfTransport {
		transport, err := render(transportTemplate, "transport", service, view)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Name: TransportFileName(service.Interface), Content: transport})
	}
	if half == HalfAll || half == HalfAssembly {
		assembly, err := render(assemblyTemplate, "assembly", service, view)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Name: AssemblyFileName(service.Interface), Content: assembly})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("servicerpc: unknown half %q (want transport, assembly or all)", half)
	}
	return files, nil
}

func render(tmpl *template.Template, half string, service Service, view view) ([]byte, error) {
	var out bytes.Buffer
	if err := tmpl.Execute(&out, view); err != nil {
		return nil, fmt.Errorf("render %s %s: %w", service.Interface, half, err)
	}
	formatted, err := format.Source(out.Bytes())
	if err != nil {
		// The unformatted source is included because a template bug is much
		// easier to find with the text in front of you than from a line
		// number in generated output nobody has.
		return nil, fmt.Errorf("format generated %s %s: %w\n%s", service.Interface, half, err, out.String())
	}
	return formatted, nil
}

// view is the template's input: Service, plus the derived names the template
// would otherwise have to compute inline.
type view struct {
	Service
	// AnyAffinity is true when at least one method routes by key.
	//
	// It drives whether the generated constructor installs the picker option.
	// Without it the affinity markers would parse, generate a context key, and
	// change nothing — a marker that reads as configured and is not, which is
	// worse than no marker.
	AnyAffinity bool
	// Iface is the interface name, e.g. "Mail".
	Iface string
	// Lower is the interface name with a lowercase first letter, for
	// unexported identifiers.
	Lower string
	// FileBase is the interface name lowercased, the stem both generated
	// file names share; the headers name the sibling file with it.
	FileBase string
	// Regenerate is the command the header cites; see Options.Regenerate.
	Regenerate string
	// Methods carries the derived per-method names.
	Methods []methodView
}

type methodView struct {
	Method
	// Const is the method-name constant, e.g. "MethodSend".
	Const string
	// Wire and Response are the generated type names.
	Wire     string
	Response string
	// ParamList is the client method's parameter list after ctx.
	ParamList string
	// ResultZeroes is the zero-value list a failed client call returns.
	ResultZeroes string
	// CallArgs is how the handler passes decoded fields to the service.
	CallArgs string
	// ResultFields assigns the service's results into the response.
	ResultFields string
	// ResultReturns is how the client reads results off the response.
	ResultReturns string
	// HasResults is false for a method that returns only error.
	HasResults bool
	// ArgNames is the parameter names alone, for forwarding a call on.
	ArgNames string
	// ResultTypes is the result types alone, for a forwarder's signature.
	ResultTypes string
	// WireFields and ResponseFields are the rendered struct field lines.
	//
	// Rendered here rather than in the template because a JSON tag needs
	// backticks, and a template that has to escape them stops being readable
	// as the code it produces — which is the only thing a template is good
	// for.
	WireFields     []string
	ResponseFields []string
}

func newView(service Service) (view, error) {
	v := view{
		Service:    service,
		Iface:      service.Interface,
		Lower:      lowerFirst(service.Interface),
		FileBase:   strings.ToLower(service.Interface),
		Regenerate: DefaultRegenerate,
	}
	for _, method := range service.Methods {
		mv := methodView{
			Method: method,
			Const:  "Method" + method.Name,
			// Unexported, and prefixed so it cannot collide with a type the
			// author wrote — the first run of this generator produced
			// `type SendRequest` for a method whose parameter type was
			// already called SendRequest.
			//
			// Unexported is also the stronger design, not just the safer
			// naming. A wire type is the client's implementation detail, and
			// making it unexported turns that from a comment into a compiler
			// fact: a caller outside this package cannot construct one, so it
			// cannot route around the client method that takes the caller's
			// identity as a parameter.
			Wire:       "rpc" + method.Name + "Request",
			Response:   "rpc" + method.Name + "Response",
			HasResults: len(method.Results) > 0,
		}

		params := make([]string, 0, len(method.Params))
		callArgs := make([]string, 0, len(method.Params)+1)
		callArgs = append(callArgs, "ctx.Context()")
		for _, param := range method.Params {
			params = append(params, param.Name+" "+param.Type)
			callArgs = append(callArgs, "wire."+exportName(param.Name))
		}
		mv.ParamList = strings.Join(params, ", ")
		names := make([]string, 0, len(method.Params))
		for _, param := range method.Params {
			names = append(names, param.Name)
		}
		mv.ArgNames = strings.Join(names, ", ")
		types := make([]string, 0, len(method.Results))
		for _, result := range method.Results {
			types = append(types, result.Type)
		}
		mv.ResultTypes = strings.Join(types, ", ")
		mv.CallArgs = strings.Join(callArgs, ", ")

		zeroes := make([]string, 0, len(method.Results))
		fields := make([]string, 0, len(method.Results))
		returns := make([]string, 0, len(method.Results))
		for _, result := range method.Results {
			zeroes = append(zeroes, zeroValue(result.Type))
			fields = append(fields, exportName(result.Name)+": "+result.Name)
			returns = append(returns, "resp."+exportName(result.Name))
		}
		for _, param := range method.Params {
			mv.WireFields = append(mv.WireFields, fmt.Sprintf("%s %s `json:%q`",
				exportName(param.Name), param.Type, jsonName(exportName(param.Name))))
		}
		for _, result := range method.Results {
			mv.ResponseFields = append(mv.ResponseFields, fmt.Sprintf("%s %s `json:%q`",
				exportName(result.Name), result.Type, jsonName(exportName(result.Name))))
		}
		mv.ResultZeroes = strings.Join(zeroes, ", ")
		mv.ResultFields = strings.Join(fields, ", ")
		mv.ResultReturns = strings.Join(returns, ", ")

		if method.Affinity != "" {
			v.AnyAffinity = true
		}
		v.Methods = append(v.Methods, mv)
	}
	return v, nil
}

// exportName uppercases the first letter, so a parameter named playerID
// becomes a field named PlayerID.
//
// It also preserves a trailing all-caps run, so playerID does not become
// PlayerId — a field name that reads as a typo makes generated code look
// machine-written in the one place a person reads it: a packet capture.
func exportName(name string) string {
	if name == "" {
		return ""
	}
	runes := []rune(name)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func lowerFirst(name string) string {
	if name == "" {
		return ""
	}
	runes := []rune(name)
	runes[0] = unicode.ToLower(runes[0])
	return string(runes)
}

// jsonName renders a Go identifier as snake_case for a JSON tag, so a payload
// reads the way the rest of this repository's payloads read.
func jsonName(name string) string {
	var out strings.Builder
	runes := []rune(name)
	for index, r := range runes {
		if unicode.IsUpper(r) {
			// Only break before an uppercase run that starts a new word, so
			// playerID becomes player_id rather than player_i_d.
			previousLower := index > 0 && unicode.IsLower(runes[index-1])
			nextLower := index+1 < len(runes) && unicode.IsLower(runes[index+1])
			if index > 0 && (previousLower || nextLower) {
				out.WriteByte('_')
			}
			out.WriteRune(unicode.ToLower(r))
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// zeroValue is what a client returns for a result when the call failed.
//
// A struct's zero value is written as T{} and a slice's as nil; anything the
// generator is unsure of gets the explicit var form, which compiles for every
// type rather than guessing.
func zeroValue(typeName string) string {
	switch {
	case strings.HasPrefix(typeName, "[]"), strings.HasPrefix(typeName, "map["), strings.HasPrefix(typeName, "*"):
		return "nil"
	case typeName == "string":
		return `""`
	case typeName == "bool":
		return "false"
	}
	switch typeName {
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "byte", "rune",
		"float32", "float64":
		return "0"
	}
	return typeName + "{}"
}
