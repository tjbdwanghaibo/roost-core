package roost

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
 accessplayer "example.com/planet/internal/access/player"
 "example.com/planet/game/player_agent"
 "example.com/planet/protocol/pb"
)
func TestClientSDKRealGeneratedTCP(t *testing.T) {
 registry:=player_agent.NewProtocolRegistry()
 transport:=&Runtime{protocols:registry}
 var syncPayload []byte
 raw,err:=os.ReadFile(os.Getenv("ROOST_CLIENT_GOLDEN"));if err!=nil{t.Fatal(err)}
 var fixtures []struct{Name string;Hex string};if err:=json.Unmarshal(raw,&fixtures);err!=nil{t.Fatal(err)}
 for _,entry:=range fixtures {if entry.Name=="sync"{data,err:=hex.DecodeString(entry.Hex);if err!=nil{t.Fatal(err)};packets,err:=wire.Decode(data,0);if err!=nil{t.Fatal(err)};syncPayload=packets[0].Payload}}
 if len(syncPayload)==0{t.Fatal("missing Go Sync fixture")}
 err=player_agent.RegisterProtocol[*pb.AddExpRequest,*pb.AddExpResponse](registry,42,43,pb.UnmarshalAddExpRequest,pb.MarshalAddExpResponse,
 func(ctx *player_agent.Context,req *pb.AddExpRequest)(*pb.AddExpResponse,error){
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
