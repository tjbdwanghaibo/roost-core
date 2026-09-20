package nest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFile(t *testing.T) {
	path := filepath.Join("testdata", "handler.go")
	funcs, pkg, err := parseFile(path)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if pkg != "testdata" {
		t.Fatalf("expected package testdata, got %s", pkg)
	}
	if len(funcs) != 6 {
		t.Fatalf("expected 6 functions, got %d", len(funcs))
	}

	// handlerPlayerLogin: single entity, one param
	f := funcs[0]
	if f.RawName != "handlerPlayerLogin" {
		t.Fatalf("expected handlerPlayerLogin, got %s", f.RawName)
	}
	if len(f.Entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(f.Entities))
	}
	if f.Entities[0].Type != "IPlayerEntity" {
		t.Fatalf("expected IPlayerEntity, got %s", f.Entities[0].Type)
	}
	if f.Entities[0].EntityKind != "" || f.Entities[0].EntityCategory != "" {
		t.Fatalf("business kind/category must not be inferred, got %+v", f.Entities[0])
	}
	if len(f.Params) != 1 || f.Params[0].Type != "string" {
		t.Fatalf("expected 1 param of type string, got %v", f.Params)
	}

	// handlerPlayerGetLevel: single entity, return + error
	f = funcs[1]
	if !f.Ret.Have || f.Ret.Type != "int32" {
		t.Fatalf("expected ret int32, got %+v", f.Ret)
	}
	if !f.Err.Have {
		t.Fatal("expected error return")
	}

	// handlerTransferItem: multi entity
	f = funcs[2]
	if len(f.Entities) != 2 {
		t.Fatalf("expected 2 entities, got %d", len(f.Entities))
	}
	if len(f.Params) != 2 {
		t.Fatalf("expected 2 params, got %d", len(f.Params))
	}
	if f.Rollback != "undo" {
		t.Fatalf("expected undo rollback, got %q", f.Rollback)
	}
	if f.Durability != "strict" {
		t.Fatalf("expected strict durability, got %q", f.Durability)
	}

	// handlerBroadcastToGroup: group entity
	f = funcs[3]
	if len(f.Entities) != 2 {
		t.Fatalf("expected 2 entities, got %d", len(f.Entities))
	}
	if !f.Entities[0].IsGroup {
		t.Fatal("first entity should be group")
	}
	if f.Entities[1].IsGroup {
		t.Fatal("second entity should not be group")
	}

	// handlerGroupCalc: group with return
	f = funcs[4]
	if !f.Entities[0].IsGroup {
		t.Fatal("first entity should be group")
	}
	if !f.Ret.Have || f.Ret.Type != "int64" {
		t.Fatalf("expected ret int64, got %+v", f.Ret)
	}

	f = funcs[5]
	if f.RawName != "handlerRemoteView" {
		t.Fatalf("expected handlerRemoteView, got %s", f.RawName)
	}
	if len(f.RemoteAccess) != 2 {
		t.Fatalf("remote access count = %d, want 2", len(f.RemoteAccess))
	}
	remote := f.RemoteAccess[0]
	if remote.Alias != "target_player" || remote.ParamName != "req" || remote.RefExpr != "req.TargetPlayerViewRef" {
		t.Fatalf("remote access = %+v", remote)
	}
	if remote.Consistency != "monotonic" || remote.Scope != "" || remote.Type != "view.PlayerViewMapSnapshot" || !remote.Required || remote.CacheTTLMillis != "" {
		t.Fatalf("remote policy = %+v", remote)
	}
	remote = f.RemoteAccess[1]
	if remote.Alias != "live_player" || remote.ParamName != "req" || remote.RefExpr != "req.LivePlayerViewRef" {
		t.Fatalf("read-only remote access = %+v", remote)
	}
	if remote.Consistency != "strong" || remote.Type != "view.PlayerViewMapSnapshot" || remote.Required {
		t.Fatalf("read-only remote policy = %+v", remote)
	}
}

func TestValidateTransactionOptionsRejectsLegacyDirty(t *testing.T) {
	if err := validateTransactionOptions("dirty", "memory"); err == nil {
		t.Fatal("legacy rollback=dirty must be rejected")
	}
}

func TestParseFileReadsRemoteRequestTagsFromPackageFiles(t *testing.T) {
	dir := t.TempDir()
	handlerPath := filepath.Join(dir, "handler.go")
	requestPath := filepath.Join(dir, "request.go")
	if err := os.WriteFile(handlerPath, []byte(`package splitreq

type IPlayerEntity interface{ ID() int64 }

//roost:nest
func handlerRemoteView(p IPlayerEntity, req RemoteViewRequest) {}
`), 0644); err != nil {
		t.Fatalf("write handler: %v", err)
	}
	if err := os.WriteFile(requestPath, []byte(`package splitreq

import "github.com/tjbdwanghaibo/roost-core/entity"

type RemoteViewRequest struct {
	TargetPlayerViewRef entity.RemoteViewRef `+"`remote:\"view.PlayerViewMapSnapshot,required\"`"+`
}
`), 0644); err != nil {
		t.Fatalf("write request: %v", err)
	}

	funcs, pkg, err := parseFile(handlerPath)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if pkg != "splitreq" {
		t.Fatalf("pkg = %q, want splitreq", pkg)
	}
	if len(funcs) != 1 {
		t.Fatalf("funcs = %d, want 1", len(funcs))
	}
	if len(funcs[0].RemoteAccess) != 1 {
		t.Fatalf("remote access count = %d, want 1", len(funcs[0].RemoteAccess))
	}
	access := funcs[0].RemoteAccess[0]
	if access.Alias != "target_player" || access.ParamName != "req" || access.RefExpr != "req.TargetPlayerViewRef" || !access.Required {
		t.Fatalf("remote access = %+v", access)
	}
}

func TestGenerate(t *testing.T) {
	path := filepath.Join("testdata", "handler.go")
	funcs, pkg, err := parseFile(path)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}

	outFile := filepath.Join(t.TempDir(), "handler_nest_gen.go")
	changed, err := generate(funcs, pkg, outFile, true, false, "RegisterHandlerNestHandlers")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !changed {
		t.Fatal("expected file to be generated")
	}

	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	src := string(content)

	// Check key patterns exist
	checks := []string{
		"func invokePlayerLogin(",
		"func RegisterHandlerNestHandlers()",
		`"github.com/tjbdwanghaibo/cube/game/view"`,
		"nest.NewParamTypeMismatchError",
		`nest.MustRegisterHandlerWithMeta(handlerNamePlayerLogin`,
		`nest.MustRegisterHandlerWithMeta(handlerNameTransferItem`,
		`nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict}`,
		`func (req RemoteViewRequest) RemoteAccess() []nest.RemoteAccess`,
		`nest.RemoteKey[view.PlayerViewMapSnapshot]{Alias: "target_player"}`,
		`func (req RemoteViewRequest) TargetPlayer() (view.PlayerViewMapSnapshot, bool)`,
		`func (req RemoteViewRequest) MustTargetPlayer() view.PlayerViewMapSnapshot`,
		`nest.RemoteKey[view.PlayerViewMapSnapshot]{Alias: "live_player"}`,
		`func (req RemoteViewRequest) LivePlayer() (view.PlayerViewMapSnapshot, bool)`,
		`entity.RemoteReadMonotonic`,
		`entity.RemoteReadLinearizable`,
		`req.TargetPlayerViewRef`,
		`req.LivePlayerViewRef`,
		`nest.RemoteScopeOf[view.PlayerViewMapSnapshot]()`,
		`CacheTTLMillis: 0`,
	}
	for _, check := range checks {
		if !strings.Contains(src, check) {
			t.Errorf("output missing: %s", check)
		}
	}
	if strings.Contains(src, `Mode: nest.RemoteAcquire`) {
		t.Error("V2 generated remote access must not use legacy acquire modes")
	}
	for _, check := range []string{
		"func Broadcast_PlayerLogin(",
		"func Send_PlayerLogin(",
	} {
		if strings.Contains(src, check) {
			t.Errorf("handler output should not contain sender API: %s", check)
		}
	}

	senderFile := filepath.Join(t.TempDir(), "handler_nest_gen.go")
	changed, err = generate(funcs, pkg+"_sender", senderFile, true, true, "")
	if err != nil {
		t.Fatalf("generate sender: %v", err)
	}
	if !changed {
		t.Fatal("expected sender file to be generated")
	}
	senderContent, err := os.ReadFile(senderFile)
	if err != nil {
		t.Fatalf("read sender output: %v", err)
	}
	senderSrc := string(senderContent)
	for _, check := range []string{
		"func (s *NestSender) Broadcast_PlayerLogin(ctx context.Context,",
		"func (s *NestSender) Send_PlayerLogin(ctx context.Context,",
		"func (s *NestSender) Delay_PlayerLogin(ctx context.Context,",
		"func (s *NestSender) MultiSend_TransferItem(ctx context.Context,",
		"func (s *NestSender) MultiDelay_TransferItem(ctx context.Context,",
		"func (s *NestSender) MultiGroupSend_BroadcastToGroup(ctx context.Context,",
		"func (s *NestSender) MultiGroupDelay_BroadcastToGroup(ctx context.Context,",
	} {
		if !strings.Contains(senderSrc, check) {
			t.Errorf("sender output missing: %s", check)
		}
	}
	for _, check := range []string{
		"func Sync_PlayerGetLevel(",
		"func MultiGroupSync_GroupCalc(",
	} {
		if strings.Contains(senderSrc, check) {
			t.Errorf("async sender output should not contain sync API: %s", check)
		}
	}
	if strings.Contains(senderSrc, "func invokePlayerLogin(") || strings.Contains(senderSrc, "MustRegisterHandler") {
		t.Fatal("sender output should not contain invoke wrapper or registration")
	}
	if strings.Contains(senderSrc, "SendOptionSetEntity") {
		t.Fatal("sender output should not contain entity metadata send options")
	}

	syncSenderFile := filepath.Join(t.TempDir(), "handler_nest_gen.go")
	changed, err = generateSyncSender(funcs, pkg+"_syncsender", syncSenderFile, true)
	if err != nil {
		t.Fatalf("generate syncsender: %v", err)
	}
	if !changed {
		t.Fatal("expected syncsender file to be generated")
	}
	syncSenderContent, err := os.ReadFile(syncSenderFile)
	if err != nil {
		t.Fatalf("read syncsender output: %v", err)
	}
	syncSenderSrc := string(syncSenderContent)
	for _, check := range []string{
		"\"context\"",
		"func (s *NestSender) Sync_PlayerGetLevel(ctx context.Context,",
		"func (s *NestSender) MultiGroupSync_GroupCalc(ctx context.Context,",
		"client.Request(ctx, handlerNamePlayerGetLevel,",
		"client.RequestMultiGroup(ctx, handlerNameGroupCalc,",
	} {
		if !strings.Contains(syncSenderSrc, check) {
			t.Errorf("syncsender output missing: %s", check)
		}
	}
	for _, check := range []string{
		"func Broadcast_PlayerLogin(",
		"func Send_PlayerLogin(",
		"func Delay_PlayerLogin(",
		"func MultiSend_TransferItem(",
		"func MultiGroupSend_BroadcastToGroup(",
	} {
		if strings.Contains(syncSenderSrc, check) {
			t.Errorf("syncsender output should not contain async API: %s", check)
		}
	}

	// Idempotent check
	changed, err = generate(funcs, pkg, outFile, false, false, "RegisterHandlerNestHandlers")
	if err != nil {
		t.Fatalf("generate again: %v", err)
	}
	if changed {
		t.Fatal("expected no change on re-run")
	}
}

func TestGenerateMultipleNonErrorReturns(t *testing.T) {
	path := writeTempGoFile(t, `package multiret

type IPlayerEntity interface{ ID() int64 }

//roost:nest
func handlerLookup(p IPlayerEntity) (string, bool) { return "", false }
`)
	funcs, pkg, err := parseFile(path)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(funcs) != 1 || len(funcs[0].Returns) != 2 {
		t.Fatalf("returns = %+v, want two values", funcs[0].Returns)
	}

	handlerFile := filepath.Join(t.TempDir(), "handler_nest_gen.go")
	if _, err = generate(funcs, pkg, handlerFile, true, false, "RegisterHandlers"); err != nil {
		t.Fatalf("generate handler: %v", err)
	}
	handlerContent, err := os.ReadFile(handlerFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`r0, r1 := handlerLookup(e0)`,
		`ret = []any{r0, r1}`,
	} {
		if !strings.Contains(string(handlerContent), want) {
			t.Errorf("handler output missing %q", want)
		}
	}

	senderFile := filepath.Join(t.TempDir(), "sender_nest_gen.go")
	if _, err = generateSyncSender(funcs, pkg+"_syncsender", senderFile, true); err != nil {
		t.Fatalf("generate sync sender: %v", err)
	}
	senderContent, err := os.ReadFile(senderFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`func (s *SenderSender) Sync_Lookup(ctx context.Context, id int64) (ret0 string, ret1 bool, err error)`,
		`retValues, ok := retXXX.([]any)`,
		`ret0, ok = retValues[0].(string)`,
		`ret1, ok = retValues[1].(bool)`,
	} {
		if !strings.Contains(string(senderContent), want) {
			t.Errorf("sync sender output missing %q", want)
		}
	}
}

func TestRoostMarkerExplicitCapabilityAndInjectedSender(t *testing.T) {
	path := writeTempGoFile(t, `package capability

type BagOwner interface { Bag() any }

//roost:nest target=player sync
func handlerUse(owner BagOwner, itemID int64) {}
`)
	funcs, pkg, err := parseFile(path)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(funcs) != 1 || len(funcs[0].Entities) != 1 {
		t.Fatalf("funcs=%+v", funcs)
	}
	if got := funcs[0].Entities[0]; got.Type != "BagOwner" || got.Target != "player" {
		t.Fatalf("entity=%+v", got)
	}
	if !funcs[0].Sync {
		t.Fatal("sync marker was not retained")
	}

	asyncFile := filepath.Join(t.TempDir(), "handler_bag_nest_gen.go")
	if _, err = generate(funcs, pkg+"_sender", asyncFile, true, true, ""); err != nil {
		t.Fatalf("generate async sender: %v", err)
	}
	asyncRaw, err := os.ReadFile(asyncFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"type BagSender struct",
		"func NewBagSender(client nest.Client) *BagSender",
		"func (s *BagSender) Send_Use(ctx context.Context, id int64, itemID int64) error",
		"return client.Dispatch(ctx, handlerNameUse, id, nest.NewParams(itemID))",
	} {
		if !strings.Contains(string(asyncRaw), want) {
			t.Errorf("async sender missing %q:\n%s", want, asyncRaw)
		}
	}

	syncFile := filepath.Join(t.TempDir(), "handler_bag_nest_gen.go")
	if _, err = generateSyncSender(funcs, pkg+"_syncsender", syncFile, true); err != nil {
		t.Fatalf("generate sync sender: %v", err)
	}
	syncRaw, err := os.ReadFile(syncFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"func (s *BagSender) Sync_Use(ctx context.Context, id int64, itemID int64) (err error)",
		"client.Request(ctx, handlerNameUse, id, nest.NewParams(itemID))",
	} {
		if !strings.Contains(string(syncRaw), want) {
			t.Errorf("sync sender missing %q:\n%s", want, syncRaw)
		}
	}
}

func TestRoostMarkerRejectsInvalidTargetDeclaration(t *testing.T) {
	path := writeTempGoFile(t, `package capability
type BagOwner interface { Bag() any }
//roost:nest targets=player,alliance
func handlerBad(owner BagOwner) {}
`)
	if _, _, err := parseFile(path); err == nil || !strings.Contains(err.Error(), "targets declares 2") {
		t.Fatalf("error=%v", err)
	}
}

func TestRoostMethodHandlerInjectsReceiver(t *testing.T) {
	path := writeTempGoFile(t, `package capability
type BagOwner interface { Bag() any }
type Handler struct{}
//roost:nest target=player
func (h *Handler) handlerUse(owner BagOwner, itemID int64) error { return nil }
`)
	funcs, pkg, err := parseFile(path)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(funcs) != 1 {
		t.Fatalf("funcs=%+v", funcs)
	}
	fn := funcs[0]
	if fn.ReceiverType != "*Handler" || fn.InvokeName != "receiver.handlerUse" || fn.RawName != "Handler.handlerUse" {
		t.Fatalf("method metadata=%+v", fn)
	}
	out := filepath.Join(t.TempDir(), "handler_bag_nest_gen.go")
	if _, err := generate(funcs, pkg, out, true, false, "RegisterBagNestHandlers"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`nest.NewHandlerName("Handler.handlerUse")`,
		`func invokeUse(receiver *Handler, es []entity.IThreadSafeEntity`,
		`err = receiver.handlerUse(e0, p0)`,
		`func RegisterBagNestHandlers(receiver *Handler)`,
		`if receiver == nil`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("generated method handler missing %q:\n%s", want, raw)
		}
	}
}

func TestParseRejectsNonFinalOrMultipleErrorReturns(t *testing.T) {
	for _, signature := range []string{"(error, bool)", "(error, error)"} {
		path := writeTempGoFile(t, `package invalidret
type IPlayerEntity interface{ ID() int64 }
//roost:nest
func handlerInvalid(p IPlayerEntity) `+signature+` { panic("unused") }
`)
		if _, _, err := parseFile(path); err == nil || !strings.Contains(err.Error(), "single final return value") {
			t.Fatalf("signature %s error = %v", signature, err)
		}
	}
}

func TestGenerateRejectsUnknownRemoteSnapshotTypePackage(t *testing.T) {
	path := writeTempGoFile(t, `package invalid
import "github.com/tjbdwanghaibo/roost-core/entity"
type IPlayerEntity interface{ ID() int64 }
type Req struct { TargetPlayerViewRef entity.RemoteViewRef `+"`remote:\"unknownpkg.PlayerViewMapSnapshot\"`"+` }
//roost:nest
func handlerRemoteView(p IPlayerEntity, req Req) {}
`)
	funcs, pkg, err := parseFile(path)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	outFile := filepath.Join(t.TempDir(), "handler_nest_gen.go")
	_, err = generate(funcs, pkg, outFile, true, false, "RegisterHandlerNestHandlers")
	if err == nil {
		t.Fatal("generate succeeded, want unknown package alias error")
	}
	if !strings.Contains(err.Error(), "unknownpkg") {
		t.Fatalf("error = %v, want unknownpkg", err)
	}
}

func TestParseFileRejectsInvalidRemoteTags(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "write mode",
			src: `package invalid
import "github.com/tjbdwanghaibo/roost-core/entity"
type IPlayerEntity interface{ ID() int64 }
type Req struct { TargetPlayerViewRef entity.RemoteViewRef ` + "`remote:\"write,view.PlayerViewMapSnapshot\"`" + ` }
//roost:nest
func handlerBad(p IPlayerEntity, req Req) {}
`,
			want: "write",
		},
		{
			name: "non remote ref field",
			src: `package invalid
type IPlayerEntity interface{ ID() int64 }
type Req struct { TargetPlayerViewRef int64 ` + "`remote:\"view.PlayerViewMapSnapshot\"`" + ` }
//roost:nest
func handlerBad(p IPlayerEntity, req Req) {}
`,
			want: "entity.RemoteViewRef",
		},
		{
			name: "duplicate alias",
			src: `package invalid
import "github.com/tjbdwanghaibo/roost-core/entity"
type IPlayerEntity interface{ ID() int64 }
type Req struct {
	TargetPlayerViewRef entity.RemoteViewRef ` + "`remote:\"view.PlayerViewMapSnapshot\"`" + `
	TargetPlayerRef entity.RemoteViewRef ` + "`remote:\"view.PlayerViewMapSnapshot\"`" + `
}
//roost:nest
func handlerBad(p IPlayerEntity, req Req) {}
`,
			want: "duplicate remote alias",
		},
		{
			name: "unknown option",
			src: `package invalid
import "github.com/tjbdwanghaibo/roost-core/entity"
type IPlayerEntity interface{ ID() int64 }
type Req struct { TargetPlayerViewRef entity.RemoteViewRef ` + "`remote:\"view.PlayerViewMapSnapshot,requried\"`" + ` }
//roost:nest
func handlerBad(p IPlayerEntity, req Req) {}
`,
			want: "unknown remote tag option",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempGoFile(t, tt.src)
			_, _, err := parseFile(path)
			if err == nil {
				t.Fatal("parseFile succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func writeTempGoFile(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "handler.go")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatalf("write temp go file: %v", err)
	}
	return path
}

func TestGenerateBootstrapNest(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "nest.go")
	regs := []bootstrapRegistration{
		{
			ImportPath:   "github.com/tjbdwanghaibo/cube/game/components/player_login/handler",
			RegisterFunc: "RegisterHandlerNestHandlers",
		},
		{
			ImportPath:   "github.com/tjbdwanghaibo/cube/game/entities/player/handler",
			RegisterFunc: "RegisterHandlerBagNestHandlers",
		},
		{
			ImportPath:   "github.com/tjbdwanghaibo/cube/game/entities/player/handler",
			RegisterFunc: "RegisterHandlerNestHandlers",
		},
	}
	changed, err := generateBootstrapNest(regs, outFile, true)
	if err != nil {
		t.Fatalf("generateBootstrapNest: %v", err)
	}
	if !changed {
		t.Fatal("expected bootstrap file to be generated")
	}

	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	src := string(content)
	for _, check := range []string{
		"// Code generated by tool/nest. DO NOT EDIT.",
		"package registry",
		`playerloginhandler "github.com/tjbdwanghaibo/cube/game/components/player_login/handler"`,
		`playerhandler "github.com/tjbdwanghaibo/cube/game/entities/player/handler"`,
		"//roost:register phase=nest",
		"playerloginhandler.RegisterHandlerNestHandlers()",
		"playerhandler.RegisterHandlerBagNestHandlers()",
		"playerhandler.RegisterHandlerNestHandlers()",
	} {
		if !strings.Contains(src, check) {
			t.Fatalf("bootstrap output missing: %s\n%s", check, src)
		}
	}
	if strings.Count(src, `"github.com/tjbdwanghaibo/cube/game/entities/player/handler"`) != 1 {
		t.Fatalf("player handler package should be imported once:\n%s", src)
	}

	changed, err = generateBootstrapNest(regs, outFile, false)
	if err != nil {
		t.Fatalf("generateBootstrapNest again: %v", err)
	}
	if changed {
		t.Fatal("expected no change on re-run")
	}
}
