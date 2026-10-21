package roost

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func generatedTestBodies(t *testing.T, root, rel string) map[string]string {
	t.Helper()
	source := readProjectFile(t, root, rel)
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Base(rel), source, 0)
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	bodies := map[string]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		// 单文件解析，FileSet 里只有这一个文件，Pos 减 1 就是字节偏移。
		bodies[fn.Name.Name] = source[fn.Body.Lbrace-1 : fn.Body.Rbrace]
	}
	return bodies
}

// firstIndex 返回 needles 里任一个在 body 中第一次出现的位置，都没有时返回 -1。
func firstIndex(body string, needles ...string) int {
	first := -1
	for _, needle := range needles {
		if at := strings.Index(body, needle); at >= 0 && (first < 0 || at < first) {
			first = at
		}
	}
	return first
}

func TestGeneratedPlayerTCPTestsWaitForTheSessionsTheyUse(t *testing.T) {
	t.Parallel()
	// 通用运行测试随实现迁到 gateway；保持相同等待契约，不能靠生成物复制测试。
	root := "../../.."
	rel := "infra/network/gateway/tcp_test.go"
	bodies := generatedTestBodies(t, root, rel)

	// 助手缺失时照样往下查每个用例，失败信息落在“哪个用例没等”上。
	helper, ok := bodies["waitRegistered"]
	if !ok {
		t.Errorf("%s has no waitRegistered helper", rel)
	}
	for _, want := range []string{"runtime.ActiveSessions(playerID) != want", "time.Now().Add(2 * time.Second)", "t.Fatalf("} {
		if ok && !strings.Contains(helper, want) {
			t.Errorf("waitRegistered is not a bounded wait on the registered count: missing %q", want)
		}
	}

	dials := []string{"dialAuthenticated(t, server,", "stalled(t, server,"}
	uses := []string{"runtime.PushPlayer(", "runtime.PushSession(", "runtime.CloseSessions(", "runtime.ActiveSessions(", "fillUntilOneFails("}
	checked := map[string]bool{}
	for name, body := range bodies {
		if !strings.HasPrefix(name, "Test") {
			continue
		}
		use := firstIndex(body, uses...)
		if use < 0 || firstIndex(body, dials...) < 0 {
			continue
		}
		lastDial := -1
		for _, dial := range dials {
			lastDial = max(lastDial, strings.LastIndex(body[:use], dial))
		}
		wait := strings.Index(body, "waitRegistered(t, runtime, ")
		if wait < 0 || wait < lastDial || wait > use {
			t.Errorf("%s pushes, closes or counts the sessions it dialed without first waiting for them to be registered "+
				"(last dial at %d, wait at %d, first use at %d): the server acks before it registers", name, lastDial, wait, use)
		}
		checked[name] = true
	}
	for _, name := range []string{
		"TestAConnectionThatCannotTakeAPushIsClosedAndTheOthersKeepIt",
		"TestAPushNoConnectionTakesFailsAndClosesThemAll",
		"TestAPushThatFailsBeforeWritingClosesNoConnection",
		"TestASessionPushThatCannotBeWrittenClosesThatSession",
		"TestCloseSessionsCountsTheSessionsItClosed",
		"TestAnExpiringCallerDeadlineClosesNoHealthyConnection",
	} {
		if !checked[name] {
			t.Errorf("%s was not checked: it no longer dials and then uses the runtime, or it is gone", name)
		}
	}
}

func TestGeneratedSceneConnectionTestsWaitForEveryDialedPlayer(t *testing.T) {
	t.Parallel()
	root, err := generatedProjectFixture("game-demo")
	if err != nil {
		t.Fatal(err)
	}
	rel := "internal/service/game/scene_connections_test.go"
	bodies := generatedTestBodies(t, root, rel)
	dial := regexp.MustCompile(`dialScenePlayer\(t, addr, (\w+),`)
	uses := []string{"joinReady(", "stallPlayer(", "transport.PushPlayer("}
	dialed := 0
	for name, body := range bodies {
		if !strings.HasPrefix(name, "Test") {
			continue
		}
		for _, match := range dial.FindAllStringSubmatchIndex(body, -1) {
			at, player := match[1], body[match[2]:match[3]]
			dialed++
			next := firstIndex(body[at:], uses...)
			if next < 0 {
				continue
			}
			if !strings.Contains(body[at:at+next], "waitActiveSessions(t, transport, "+player+", ") {
				t.Errorf("%s dials %s and uses the scene before waiting for that connection to be registered: "+
					"the transport acks before it registers, and a member without a registered connection is swept out", name, player)
			}
		}
	}
	if dialed < 6 {
		t.Errorf("%s: found %d dials, want the 6 of the two stalled-connection tests", rel, dialed)
	}
}

const playerTCPStopContractTest = `package tcp

import (
	"context"
	"testing"

	"example.com/planet/internal/stopcontract"
 "github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
)

// Server.Stop 自己不持有排空之后要释放的依赖：App 只在它返回 nil 之后继续停后面的 Mod。
func TestA3GeneratedServerStopContract(t *testing.T) {
	var server *gateway.TCPServer
	var release chan struct{}
	var returned <-chan struct{}
	var stop func(context.Context) error
	var released func() bool
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			server, release, _, returned = stalledAuthServer(t)
			stop, released = stopcontract.CallerReleases(server.Stop)
		},
		Stop:     func(ctx context.Context) error { return stop(ctx) },
		Release:  func() { close(release) },
		Released: func() bool { return released() },
	})
	select {
	case <-returned:
	default:
		t.Fatal("Stop reported success while the authenticator was still running")
	}
}

// Mod.StopWithContext 在服务器排空之后才停会话关闭派发器（Runtime 的生命周期 goroutine）。
func TestA3GeneratedModStopContract(t *testing.T) {
	var mod *Mod
	var release chan struct{}
	runtime := NewRuntime(nil,defaultLoginTimeout)
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			var server *gateway.TCPServer
			server, release, _, _ = stalledAuthServer(t)
			mod = &Mod{config: defaultConfig(), server: server, transportRuntime: runtime}
		},
		Stop:    func(ctx context.Context) error { return mod.StopWithContext(ctx) },
		Release: func() { close(release) },
		Released: func() bool {
			return mod.server == nil
		},
	})
}
`

func TestGeneratedPlayerTCPStopContract(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs the generated game-demo player TCP package")
	}
	t.Parallel()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := copyOfNewProject(t, "game-demo")
	goModPath := filepath.Join(root, "go.mod")
	goMod, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatal(err)
	}
	replace := regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`)
	goMod = replace.ReplaceAll(goMod, nil)
	goMod = append(goMod, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
	skeleton, err := os.ReadFile(filepath.Join(repo, "internal", "stopcontract", "stopcontract.go"))
	if err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string][]byte{
		"go.mod":                                goMod,
		"internal/stopcontract/stopcontract.go": skeleton,
		"internal/access/player/tcp/a3_stop_contract_test.go": []byte(playerTCPStopContractTest),
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	goName := "go"
	if runtime.GOOS == "windows" {
		goName += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", goName), "test", "-mod=mod", "-count=1", "-v", "-run", "TestA3Generated", "./internal/access/player/tcp/")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated player TCP stop contract: %v\n%s", err, out)
	}
	for _, name := range []string{"TestA3GeneratedServerStopContract", "TestA3GeneratedModStopContract"} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run and pass:\n%s", name, out)
		}
	}
}

// D16：生成的嵌入 TCP Mod 在 Provide 时把 entity.kick 挂到 App 的 admin 注册表；生成测试经正式 TCP 连接验证踢下线
// 真的关掉了客户端连接。只编译并运行这一个生成测试。
func TestGeneratedPlayerTCPAdminKick(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs the generated game-demo player TCP package")
	}
	t.Parallel()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := copyOfNewProject(t, "game-demo")
	goModPath := filepath.Join(root, "go.mod")
	goMod, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatal(err)
	}
	replace := regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`)
	goMod = replace.ReplaceAll(goMod, nil)
	goMod = append(goMod, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
	if err := os.WriteFile(goModPath, goMod, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "test", "-mod=mod", "-race", "-count=1", "-v", "-run", "^TestAdminEntityKickClosesThePlayersConnections$", "./internal/access/player/tcp/")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated entity.kick: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestAdminEntityKickClosesThePlayersConnections") {
		t.Fatalf("the generated entity.kick test did not run and pass:\n%s", out)
	}
}

// 正式生成TCP+生成PB类型+真实C#客户端，不用mock连接替代协议消费。
const clientSDKGeneratedTest = `package tcp
import (
 "context"
 "encoding/binary"
 "encoding/json"
 "encoding/hex"
 "errors"
 "net"
 "os"
 "os/exec"
 "testing"
 "time"
 "github.com/tjbdwanghaibo/roost-core/client/wire"
 "github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
 "github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"
 "github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
 "sync"
 "sync/atomic"
 "github.com/tjbdwanghaibo/roost-core/robot"
 accessplayer "example.com/planet/internal/access/player"
 "example.com/planet/game/playeraccess"
 "example.com/planet/protocol/pb"
)
func TestClientSDKRealGeneratedTCP(t *testing.T) {
 registry:=playeraccess.NewProtocolRegistry()
 transport:=NewRuntime(registry,defaultLoginTimeout)
 // 仅测试夹具隐藏live帧；生产TCPSender不会丢帧。实时通道只准入，追帧通道入队并等写出。
 // offline期间丢弃全部下行，模拟客户端断线超过历史窗口；快照分块与恢复失败通知不是C7，原样放行。
 var offline atomic.Bool
 hideLive:=func(push func(context.Context,string,uint32,[]byte)error)func(context.Context,string,uint32,[]byte)error{
  return func(ctx context.Context,id string,msg uint32,data []byte)error{
   if offline.Load(){return nil}
   if lockstep.IsSnapshotChunk(data)||lockstep.IsCatchupNotice(data){return push(ctx,id,msg,data)}
   frames,err:=lockstep.DecodeBroadcast(data);if err!=nil{return err}
   if last:=frames[len(frames)-1].ID;last>1 && last<10 {return nil}
   return push(ctx,id,msg,data)
  }
 }
 sender,err:=lockstep.NewTCPSender(50002,wire.DefaultMaxPayload,func(id nettransport.SessionID)(string,bool){return "sdk",id==7},hideLive(transport.PushLockstepSession),hideLive(transport.TCPRuntime.PushLockstepSessionWritten))
 if err!=nil{t.Fatal(err)}
 // 服务器参考模拟：输入链hash（与C# LockstepInputHasher、robot.FrameHasher同一算法），每8帧发布一份40 KiB的不可变检查点
 // （前16字节为帧号与链值，其余是可推导的填充，让快照经真实TCP分成多块）；Provider只读已发布的检查点。
 reference:=robot.NewFrameHasher();var checkpoint atomic.Pointer[lockstep.Snapshot]
 var sumsMu sync.Mutex;sums:=map[lockstep.FrameID]uint64{};reported:=map[lockstep.FrameID]uint64{}
 provider:=lockstep.SnapshotProviderFunc(func(_ context.Context,req lockstep.SnapshotRequest)(lockstep.Snapshot,error){
  s:=checkpoint.Load();if s==nil||s.Frame<req.MinFrame||s.Frame>req.MaxFrame{return lockstep.Snapshot{},lockstep.ErrSnapshotUnavailable};return *s,nil
 })
 // 历史窗口取下限64帧，让断线超过窗口的场景只需几百帧；窗口内的缺口（帧2..9）仍按追帧补齐。
 room,err:=lockstep.NewMatch(lockstep.MatchConfig{Sequencer:lockstep.SequencerConfig{Players:[]lockstep.PlayerID{7},MaxInputBytes:16},Datagrams:sender,Reliable:sender,RedundancyDepth:1,CatchupBatchFrames:16,CatchupSendWait:time.Second,HistoryRetainFrames:lockstep.MinHistoryRetainFrames,Snapshots:lockstep.SnapshotConfig{Provider:provider}})
 if err!=nil{t.Fatal(err)};if err:=room.Attach(7,7);err!=nil{t.Fatal(err)}
 // tick在owner上切一帧并推进参考模拟；Tick的err只说明发送失败，帧已切出。
 tick:=func(ctx context.Context)error{
  f,err:=room.Tick(ctx);reference.Fold(f);sum:=reference.Sum()
  sumsMu.Lock();sums[f.ID]=sum;sumsMu.Unlock()
  if f.ID%8==0{data:=make([]byte,40<<10);binary.LittleEndian.PutUint64(data,uint64(f.ID));binary.LittleEndian.PutUint64(data[8:],sum);for i:=16;i<len(data);i++{data[i]=byte(i*31+int(f.ID))};checkpoint.Store(&lockstep.Snapshot{Frame:f.ID,Data:data,StateHash:sum,HasStateHash:true})}
  return err
 }
 type action struct {ctx context.Context; command *lockstep.Command;ticks int;fn func()error; done chan error}
 actions:=make(chan action);ownerDone:=make(chan struct{})
 go func(){defer close(ownerDone);defer room.Close();for a:=range actions {
  var err error
  if a.fn!=nil {a.done<-a.fn();continue}
  if a.command!=nil {err=room.HandleCommand(7,*a.command)}
  if err==nil {for i:=0;i<a.ticks;i++ {if err=tick(a.ctx);err!=nil{break}}}
  a.done<-err
 }}()
 defer func(){close(actions);<-ownerDone}()
 run:=func(ctx context.Context,c *lockstep.Command,ticks int)error{a:=action{ctx:ctx,command:c,ticks:ticks,done:make(chan error,1)};select{case actions<-a:case <-ctx.Done():return ctx.Err()};return <-a.done}
 own:=func(ctx context.Context,fn func()error)error{a:=action{fn:fn,done:make(chan error,1)};select{case actions<-a:case <-ctx.Done():return ctx.Err()};return <-a.done}
 var lockstepDecoded atomic.Int32
 if err:=playeraccess.RegisterPayloadNotify(registry,50001,wire.PayloadLockstep,func(data []byte)(lockstep.Command,error){lockstepDecoded.Add(1);return lockstep.DecodeCommand(data)},func(ctx *playeraccess.Context,c lockstep.Command)error{
  if ctx.PlayerID!=7 {t.Error("unauthenticated seat")}
  if c.Operation==lockstep.OpHash {sumsMu.Lock();reported[c.Frame]=c.Hash;sumsMu.Unlock()}
  ticks:=0;if c.Operation==lockstep.OpInput || c.Operation==lockstep.OpCatchup {ticks=1}
  return run(ctx.Context(),&c,ticks)
 });err!=nil{t.Fatal(err)}
 var syncPayload []byte
 raw,err:=os.ReadFile(os.Getenv("ROOST_CLIENT_GOLDEN"));if err!=nil{t.Fatal(err)}
 var fixtures []struct{Name string;Hex string};if err:=json.Unmarshal(raw,&fixtures);err!=nil{t.Fatal(err)}
 for _,entry:=range fixtures {if entry.Name=="sync"{data,err:=hex.DecodeString(entry.Hex);if err!=nil{t.Fatal(err)};packets,err:=wire.Decode(data,0);if err!=nil{t.Fatal(err)};syncPayload=packets[0].Payload}}
 if len(syncPayload)==0{t.Fatal("missing Go Sync fixture")}
 var pbDecoded atomic.Int32
 err=playeraccess.RegisterProtocol[*pb.AddExpRequest,*pb.AddExpResponse](registry,42,43,func(data []byte)(*pb.AddExpRequest,error){pbDecoded.Add(1);return pb.UnmarshalAddExpRequest(data)},pb.MarshalAddExpResponse,
 func(ctx *playeraccess.Context,req *pb.AddExpRequest)(*pb.AddExpResponse,error){
  if req.Amount==43{if err:=run(ctx.Context(),nil,9);err!=nil{return nil,err}}
  if req.Amount==42{if err:=transport.PushSyncPlayer(ctx.Context(),7,10103,syncPayload);err!=nil{return nil,err}}
  // 快照恢复端到端：44 = 客户端断线期间切200帧（超过64帧窗口，下行全部丢弃），回最新帧号；
  // 45 = 按5ms周期切帧直到座位回到实时（取快照、分块、续帧都在这些Tick里推进），回最新帧号；
  // 46 = 客户端在最新帧上报的链值是否与服务器参考一致（1 一致）。
  head:=int32(0)
  switch req.Amount {
  case 44:
   offline.Store(true)
   err:=own(ctx.Context(),func()error{for i:=0;i<200;i++{if err:=tick(ctx.Context());err!=nil{return err}};head=int32(room.NextFrame()-1);return nil})
   offline.Store(false);if err!=nil{return nil,err}
   return &pb.AddExpResponse{LevelsGained:head},nil
  case 45:
   if err:=own(ctx.Context(),func()error{for i:=0;i<400&&room.CatchingUp(7);i++{time.Sleep(5*time.Millisecond);if err:=tick(ctx.Context());err!=nil{return err}};head=int32(room.NextFrame()-1);if room.CatchingUp(7){return errors.New("seat still recovering")};return nil});err!=nil{return nil,err}
   return &pb.AddExpResponse{LevelsGained:head},nil
  case 46:
   last:=lockstep.FrameID(0);sumsMu.Lock();for f:=range reported{last=max(last,f)};ok:=last>12&&reported[last]==sums[last];sumsMu.Unlock()
   if ok{head=1};return &pb.AddExpResponse{LevelsGained:head},nil
  }
  return &pb.AddExpResponse{LevelsGained:int32(req.Amount)},nil
 })
 if err!=nil{t.Fatal(err)};if err:=registry.Seal();err!=nil{t.Fatal(err)}
 cfg:=defaultConfig();cfg.Addr="127.0.0.1:0"
 server,err:=NewServer(cfg,&accessplayer.Runtime{Protocols:registry},AuthenticatorFunc(func(_ context.Context,token string,_ net.Addr)(gateway.Principal,error){
  if token!="ticket"{return gateway.Principal{},gateway.ErrUnauthenticated};return gateway.Principal{PlayerID:7,SessionID:"sdk"},nil
 }))
 if err!=nil{t.Fatal(err)};if err:=server.ConnectRuntime(transport.TCPRuntime);err!=nil{t.Fatal(err)};if err:=server.Start();err!=nil{t.Fatal(err)}
 defer func(){ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second);defer cancel();if err:=server.Stop(ctx);err!=nil{t.Error(err)}}()
 host,port,err:=net.SplitHostPort(server.Addr().String());if err!=nil{t.Fatal(err)}
 ctx,cancel:=context.WithTimeout(context.Background(),time.Minute);defer cancel()
 cmd:=exec.CommandContext(ctx,"dotnet",os.Getenv("ROOST_CLIENT_TEST_DLL"),os.Getenv("ROOST_CLIENT_GOLDEN"),host,port)
 out,err:=cmd.CombinedOutput();if err!=nil{t.Fatalf("C# real TCP: %v\n%s",err,out)};t.Log(string(out))
 // C#客户端经快照恢复后续帧，在最新帧上报的链值必须等于服务器参考模拟在同一帧的值。
 sumsMu.Lock();last:=lockstep.FrameID(0);for f:=range reported{last=max(last,f)};want,got:=sums[last],reported[last];sumsMu.Unlock()
 if last<=212||got!=want{t.Fatalf("C# hash after snapshot recovery: frame %d got %d want %d",last,got,want)}
 // 错误kind不能到达任何decoder，包括使用已知合法Command/PB载荷。
 for _,tc:=range []struct{flags byte;id uint32;payload []byte}{{0,50001,[]byte{0xc8,1,3,1}},{wire.FlagLockstep,42,[]byte{8,1}},{6,42,nil}} {
  beforeLS,beforePB:=lockstepDecoded.Load(),pbDecoded.Load()
  connection:=dialAuthenticated(t,server,"ticket")
  // kind11无法由合法writer封包，直接修改有效头用于负面网络验收。
  if tc.flags==6 {data,err:=wire.Encode([]*wire.Packet{{MsgID:tc.id,Seq:2}},0);if err!=nil{t.Fatal(err)};data[3]=6;if _,err:=connection.Write(data);err!=nil{t.Fatal(err)}} else if err:=wire.Write(connection,[]*wire.Packet{{Flags:tc.flags,MsgID:tc.id,Seq:2,Payload:tc.payload}},wire.DefaultMaxPayload);err!=nil{t.Fatal(err)}
  _=connection.SetReadDeadline(time.Now().Add(time.Second));if _,err:=wire.Read(connection,wire.DefaultMaxPayload);err==nil{t.Fatal("wrong kind accepted")}
  if lockstepDecoded.Load()!=beforeLS || pbDecoded.Load()!=beforePB {t.Fatal("kind mismatch reached decoder")}
  connection.Close()
 }
 // Sync请求在解码/Dispatch前拒绝；合法头不赋予客户端写权威状态的能力。
 connection:=dialAuthenticated(t,server,"ticket")
 if err:=wire.Write(connection,[]*wire.Packet{{Flags:wire.FlagSync,MsgID:42,Seq:2,Payload:[]byte{8,1}}},wire.DefaultMaxPayload);err!=nil{t.Fatal(err)}
 _=connection.SetReadDeadline(time.Now().Add(time.Second))
 if _,err:=wire.Read(connection,wire.DefaultMaxPayload);err==nil{t.Fatal("client Sync request accepted")}
}
`

func TestClientSDKAgainstGeneratedPlayerTCP(t *testing.T) {
	if testing.Short() {
		t.Skip("builds C# SDK and executes generated TCP server")
	}
	if _, err := exec.LookPath("dotnet"); err != nil {
		t.Skip(".NET SDK unavailable; Go tests do not prove C# consumption")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "dotnet", "build", filepath.Join(repo, "client/dotnet/Roost.Client.Tests/Roost.Client.Tests.csproj"), "--nologo")
	build.Env = append(os.Environ(), "DOTNET_CLI_TELEMETRY_OPTOUT=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("C# consumer build: %v\n%s", err, out)
	}
	root := copyOfNewProject(t, "game-demo")
	modPath := filepath.Join(root, "go.mod")
	mod, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatal(err)
	}
	mod = regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`).ReplaceAll(mod, nil)
	mod = append(mod, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
	if err := os.WriteFile(modPath, mod, 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "internal/access/player/tcp/client_sdk_test.go")
	if err := os.WriteFile(path, []byte(clientSDKGeneratedTest), 0644); err != nil {
		t.Fatal(err)
	}
	// 既有整工程门禁在Windows会skip；本测试实际build/vet全部生成包，不能冒认通过。
	for _, action := range []string{"build", "vet"} {
		check := exec.CommandContext(ctx, "go", action, "-mod=mod", "./...")
		check.Dir = root
		check.Env = append(os.Environ(), "GOWORK=off")
		if out, err := check.CombinedOutput(); err != nil {
			t.Fatalf("generated %s: %v\n%s", action, err, out)
		}
	}
	// 真实重生成：保护既有托管目录的字节，不依赖夹具有git仓库。
	managed := func() map[string]string {
		result := map[string]string{}
		for _, dir := range []string{"game", "protocol", "internal/access/player"} {
			err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() {
					return nil
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				result[path] = string(data)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	beforeGenerate := managed()
	generated := exec.CommandContext(ctx, "go", "generate", "./...")
	generated.Dir = root
	generated.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	if out, err := generated.CombinedOutput(); err != nil {
		t.Fatalf("generated go generate: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(beforeGenerate, managed()) {
		t.Fatal("go generate changed existing managed files")
	}
	cmd := exec.CommandContext(ctx, "go", "test", "-mod=mod", "-race", "-count=1", "-v", "./internal/access/player/tcp", "./internal/service/game", "./loadtest/playertcp")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off", "ROOST_CLIENT_TEST_DLL="+filepath.Join(repo, "client/dotnet/Roost.Client.Tests/bin/Debug/net10.0/Roost.Client.Tests.dll"), "ROOST_CLIENT_GOLDEN="+filepath.Join(repo, "client/spec/packets.json"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated consumption: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS: TestClientSDKRealGeneratedTCP") {
		t.Fatalf("C# real consumer did not run:\n%s", out)
	}
	t.Log(string(out))
}

// 注册扩展使用空业务工程，隔离game-demo真实服务依赖；不把假Nest当执行链验收。
func TestClientSDKRegistrationHook(t *testing.T) {
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := copyOfNewProject(t, "bare")
	modPath := filepath.Join(root, "go.mod")
	mod, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatal(err)
	}
	mod = regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`).ReplaceAll(mod, nil)
	mod = append(mod, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
	if err := os.WriteFile(modPath, mod, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "access", Name: "player"}); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	source := `package player_test
import (
 "testing"
 "errors"
 "github.com/tjbdwanghaibo/roost-core/framework/app"
 "github.com/tjbdwanghaibo/roost-core/framework/nest"
 "github.com/tjbdwanghaibo/roost-core/client/wire"
 "github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"
 "github.com/spf13/viper"
 "example.com/planet/game/playeraccess"
 accessplayer "example.com/planet/internal/access/player"
)
// 仅Provide测试的依赖：不模拟Nest执行，任何实际调用应失败。
type unusedNest struct{nest.Client}
func TestClientSDKRegistrationHook(t *testing.T) {
 registry:=app.NewRegistry(viper.New());if err:=registry.Register("nest",unusedNest{});err!=nil{t.Fatal(err)}
 called:=false
 mod:=accessplayer.NewMod(func(protocols *playeraccess.ProtocolRegistry,r *app.Registry)error{
  called=r==registry
  return playeraccess.RegisterPayloadNotify(protocols,50001,wire.PayloadLockstep,lockstep.DecodeCommand,func(*playeraccess.Context,lockstep.Command)error{return nil})
 })
 if err:=mod.Provide(registry);err!=nil{t.Fatal(err)}
 runtime,ok:=app.Lookup[*accessplayer.Runtime](registry,accessplayer.Name);if !ok || !called{t.Fatal("registrar not used")}
 if err:=playeraccess.RegisterNotify(runtime.Protocols,50009,lockstep.DecodeCommand,func(*playeraccess.Context,lockstep.Command)error{return nil});!errors.Is(err,playeraccess.ErrRegistrySealed){t.Fatalf("custom registry not sealed %v",err)}
 refused:=errors.New("registration failed")
 registry2:=app.NewRegistry(viper.New());if err:=registry2.Register("nest",unusedNest{});err!=nil{t.Fatal(err)}
 mod2:=accessplayer.NewMod(func(*playeraccess.ProtocolRegistry,*app.Registry)error{return refused})
 if err:=mod2.Provide(registry2);!errors.Is(err,refused){t.Fatal(err)}
 if _,ok:=app.Lookup[*accessplayer.Runtime](registry2,accessplayer.Name);ok{t.Fatal("failed registration published runtime")}
}
`
	if err := os.WriteFile(filepath.Join(root, "internal/access/player/registration_test.go"), []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-mod=mod", "-count=1", "-v", "./internal/access/player")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("registration hook: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}
