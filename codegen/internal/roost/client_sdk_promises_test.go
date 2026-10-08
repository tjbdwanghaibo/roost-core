package roost

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 正式生成TCP+生成PB类型+真实C#客户端，不用mock连接替代协议消费。
const clientSDKGeneratedTest = `package tcp
import (
 "context"
 "encoding/json"
 "encoding/hex"
 "net"
 "os"
 "os/exec"
 "testing"
 "time"
 "github.com/tjbdwanghaibo/roost-core/client/wire"
 "github.com/tjbdwanghaibo/roost-core/gateway"
 "github.com/tjbdwanghaibo/roost-core/sync/lockstep"
 "github.com/tjbdwanghaibo/roost-core/sync/nettransport"
 "sync/atomic"
 accessplayer "example.com/planet/internal/access/player"
 "example.com/planet/game/player_agent"
 "example.com/planet/protocol/pb"
)
func TestClientSDKRealGeneratedTCP(t *testing.T) {
 registry:=player_agent.NewProtocolRegistry()
 transport:=&Runtime{protocols:registry}
 sender,err:=lockstep.NewTCPSender(50002,wire.DefaultMaxPayload,func(id nettransport.SessionID)(string,bool){return "sdk",id==7},func(ctx context.Context,id string,msg uint32,data []byte)error{
  frames,err:=lockstep.DecodeBroadcast(data);if err!=nil{return err}
  // 仅测试夹具隐藏live帧；生产TCPSender不会丢帧。
  if last:=frames[len(frames)-1].ID;last>1 && last<10 {return nil}
  return transport.PushLockstepSession(ctx,id,msg,data)
 });if err!=nil{t.Fatal(err)}
 room,err:=lockstep.NewRoom(lockstep.RoomConfig{Sequencer:lockstep.SequencerConfig{Players:[]lockstep.PlayerID{7},MaxInputBytes:16},Datagrams:sender,Reliable:sender,RedundancyDepth:1,CatchupBatchFrames:16,CatchupSendWait:time.Second})
 if err!=nil{t.Fatal(err)};if err:=room.Attach(7,7);err!=nil{t.Fatal(err)}
 type action struct {ctx context.Context; command *lockstep.Command;ticks int; done chan error}
 actions:=make(chan action);ownerDone:=make(chan struct{})
 go func(){defer close(ownerDone);defer room.Close();for a:=range actions {
  var err error
  if a.command!=nil {err=room.HandleCommand(7,*a.command)}
  if err==nil {for i:=0;i<a.ticks;i++ {if _,err=room.Tick(a.ctx);err!=nil{break}}}
  a.done<-err
 }}()
 defer func(){close(actions);<-ownerDone}()
 run:=func(ctx context.Context,c *lockstep.Command,ticks int)error{a:=action{ctx:ctx,command:c,ticks:ticks,done:make(chan error,1)};select{case actions<-a:case <-ctx.Done():return ctx.Err()};return <-a.done}
 var lockstepDecoded atomic.Int32
 if err:=player_agent.RegisterPayloadNotify(registry,50001,wire.PayloadLockstep,func(data []byte)(lockstep.Command,error){lockstepDecoded.Add(1);return lockstep.DecodeCommand(data)},func(ctx *player_agent.Context,c lockstep.Command)error{
  if ctx.PlayerID!=7 {t.Error("unauthenticated seat")}
  ticks:=0;if c.Operation==lockstep.OpInput || c.Operation==lockstep.OpCatchup {ticks=1}
  return run(ctx.Context(),&c,ticks)
 });err!=nil{t.Fatal(err)}
 var syncPayload []byte
 raw,err:=os.ReadFile(os.Getenv("ROOST_CLIENT_GOLDEN"));if err!=nil{t.Fatal(err)}
 var fixtures []struct{Name string;Hex string};if err:=json.Unmarshal(raw,&fixtures);err!=nil{t.Fatal(err)}
 for _,entry:=range fixtures {if entry.Name=="sync"{data,err:=hex.DecodeString(entry.Hex);if err!=nil{t.Fatal(err)};packets,err:=wire.Decode(data,0);if err!=nil{t.Fatal(err)};syncPayload=packets[0].Payload}}
 if len(syncPayload)==0{t.Fatal("missing Go Sync fixture")}
 var pbDecoded atomic.Int32
 err=player_agent.RegisterProtocol[*pb.AddExpRequest,*pb.AddExpResponse](registry,42,43,func(data []byte)(*pb.AddExpRequest,error){pbDecoded.Add(1);return pb.UnmarshalAddExpRequest(data)},pb.MarshalAddExpResponse,
 func(ctx *player_agent.Context,req *pb.AddExpRequest)(*pb.AddExpResponse,error){
  if req.Amount==43{if err:=run(ctx.Context(),nil,9);err!=nil{return nil,err}}
  if req.Amount==42{if err:=transport.PushSyncPlayer(ctx.Context(),7,10103,syncPayload);err!=nil{return nil,err}}
  return &pb.AddExpResponse{LevelsGained:int32(req.Amount)},nil
 })
 if err!=nil{t.Fatal(err)};if err:=registry.Seal();err!=nil{t.Fatal(err)}
 cfg:=defaultConfig();cfg.Addr="127.0.0.1:0"
 server,err:=NewServer(cfg,&accessplayer.Runtime{Protocols:registry},AuthenticatorFunc(func(_ context.Context,token string,_ net.Addr)(gateway.Principal,error){
  if token!="ticket"{return gateway.Principal{},gateway.ErrUnauthenticated};return gateway.Principal{PlayerID:7,SessionID:"sdk"},nil
 }))
 if err!=nil{t.Fatal(err)};transport.server.Store(server);if err:=server.Start();err!=nil{t.Fatal(err)}
 defer func(){ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second);defer cancel();if err:=server.Stop(ctx);err!=nil{t.Error(err)}}()
 host,port,err:=net.SplitHostPort(server.listener.Addr().String());if err!=nil{t.Fatal(err)}
 ctx,cancel:=context.WithTimeout(context.Background(),time.Minute);defer cancel()
 cmd:=exec.CommandContext(ctx,"dotnet",os.Getenv("ROOST_CLIENT_TEST_DLL"),os.Getenv("ROOST_CLIENT_GOLDEN"),host,port)
 out,err:=cmd.CombinedOutput();if err!=nil{t.Fatalf("C# real TCP: %v\n%s",err,out)};t.Log(string(out))
 // 错误kind不能到达任何decoder，包括使用已知合法Command/PB载荷。
 for _,tc:=range []struct{flags byte;id uint32;payload []byte}{{0,50001,[]byte{0xc8,1,3,1}},{flagLockstep,42,[]byte{8,1}},{6,42,nil}} {
  beforeLS,beforePB:=lockstepDecoded.Load(),pbDecoded.Load()
  connection:=dialAuthenticated(t,server,"ticket")
  client:=&session{connection:connection,writeTimeout:time.Second}
  // kind11无法由合法writer封包，直接修改有效头用于负面网络验收。
  if tc.flags==6 {data,err:=wire.Encode([]*wire.Packet{{MsgID:tc.id,Seq:2}},0);if err!=nil{t.Fatal(err)};data[3]=6;if _,err:=connection.Write(data);err!=nil{t.Fatal(err)}} else if err:=client.writeFrame(context.Background(),tc.flags,tc.id,2,tc.payload);err!=nil{t.Fatal(err)}
  _=connection.SetReadDeadline(time.Now().Add(time.Second));if _,_,err:=server.readFrame(connection);err==nil{t.Fatal("wrong kind accepted")}
  if lockstepDecoded.Load()!=beforeLS || pbDecoded.Load()!=beforePB {t.Fatal("kind mismatch reached decoder")}
  connection.Close()
 }
 // Sync请求在解码/Dispatch前拒绝；合法头不赋予客户端写权威状态的能力。
 connection:=dialAuthenticated(t,server,"ticket")
 client:=&session{connection:connection,writeTimeout:time.Second}
 if err:=client.writeFrame(context.Background(),flagSync,42,2,[]byte{8,1});err!=nil{t.Fatal(err)}
 _=connection.SetReadDeadline(time.Now().Add(time.Second))
 if _,_,err:=server.readFrame(connection);err==nil{t.Fatal("client Sync request accepted")}
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
 "github.com/tjbdwanghaibo/roost-core/app"
 "github.com/tjbdwanghaibo/roost-core/nest"
 "github.com/tjbdwanghaibo/roost-core/client/wire"
 "github.com/tjbdwanghaibo/roost-core/sync/lockstep"
 "github.com/spf13/viper"
 "example.com/planet/game/player_agent"
 accessplayer "example.com/planet/internal/access/player"
)
// 仅Provide测试的依赖：不模拟Nest执行，任何实际调用应失败。
type unusedNest struct{nest.Client}
func TestClientSDKRegistrationHook(t *testing.T) {
 registry:=app.NewRegistry(viper.New());if err:=registry.Register("nest",unusedNest{});err!=nil{t.Fatal(err)}
 called:=false
 mod:=accessplayer.NewMod(func(protocols *player_agent.ProtocolRegistry,r *app.Registry)error{
  called=r==registry
  return player_agent.RegisterPayloadNotify(protocols,50001,wire.PayloadLockstep,lockstep.DecodeCommand,func(*player_agent.Context,lockstep.Command)error{return nil})
 })
 if err:=mod.Provide(registry);err!=nil{t.Fatal(err)}
 runtime,ok:=app.Lookup[*accessplayer.Runtime](registry,accessplayer.Name);if !ok || !called{t.Fatal("registrar not used")}
 if err:=player_agent.RegisterNotify(runtime.Protocols,50009,lockstep.DecodeCommand,func(*player_agent.Context,lockstep.Command)error{return nil});!errors.Is(err,player_agent.ErrRegistrySealed){t.Fatalf("custom registry not sealed %v",err)}
 refused:=errors.New("registration failed")
 registry2:=app.NewRegistry(viper.New());if err:=registry2.Register("nest",unusedNest{});err!=nil{t.Fatal(err)}
 mod2:=accessplayer.NewMod(func(*player_agent.ProtocolRegistry,*app.Registry)error{return refused})
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
