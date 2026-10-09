package roost

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// 实际生成 Entity/DAO/PB/Sender，经过真实 TCP、NATS、Nest 和文件 WAL。
// 投影记录器是测试存储，不把它记成真实 Mongo 或独立进程集群验收。
const gateConsumerTest = `package Game
import (
 "context"
 "fmt"
 "net"
 "os"
 "sync"
 "testing"
 "time"
 "github.com/spf13/viper"
 "github.com/tjbdwanghaibo/roost-core/framework/app"
 "github.com/tjbdwanghaibo/roost-core/wiring/mods"
 accessplayer "example.com/planet/internal/access/player"
 playertcp "example.com/planet/internal/access/player/tcp"
 "github.com/tjbdwanghaibo/roost-core/client/wire"
 coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
 dataengine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
 "github.com/tjbdwanghaibo/roost-core/framework/nest"
 "github.com/tjbdwanghaibo/roost-core/framework/nestwal"
 "github.com/tjbdwanghaibo/roost-core/framework/entity"
 "github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
 "github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
 "github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
 fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
 "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
 gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
 "example.com/planet/game/handler"
 syncsender "example.com/planet/game/handler/syncsender"
 "example.com/planet/game/player_agent"
 "example.com/planet/protocol/pb"
)
type gateChecker struct{matches func(context.Context,string,gateway.ProcessIdentity)(bool,error)}
func(checker gateChecker)Matches(ctx context.Context,role string,sid int32,incarnation string)(bool,error){return checker.matches(ctx,role,gateway.ProcessIdentity{ServerID:sid,Incarnation:incarnation})}
type gateProjection struct{mu sync.Mutex;records map[coredata.TransactionID]coredata.CommitRecord}
func (store *gateProjection) Project(_ context.Context,record coredata.CommitRecord)error{store.mu.Lock();defer store.mu.Unlock();store.records[record.ID]=coredata.CloneCommitRecord(record);return nil}
func TestGeneratedGateNestDAOAndWAL(t *testing.T){
 url:=os.Getenv("ROOST_DATAENGINE_IT_NATS_URL");if url==""{t.Skip("private NATS URL not configured")}
 player,world:=newScenePlayer(t,7001),newSceneWorld(t)
 handler.RegisterAddExpNestHandlers();t.Cleanup(nest.ResetHandlersForTest)
 wal,err:=nestwal.Open(nestwal.DefaultOptions(t.TempDir()));if err!=nil{t.Fatal(err)}
 store:=&gateProjection{records:make(map[coredata.TransactionID]coredata.CommitRecord)}
 projector,err:=dataengine.NewProjector(wal,store,dataengine.DefaultProjectorOptions());if err!=nil{t.Fatal(err)}
 t.Cleanup(func(){ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second);defer cancel();if err:=projector.Close(ctx);err!=nil{t.Error(err)}})
 syncTransport:=&gatewiring.Transport{SyncMessageID:501,LockstepMessageID:502}
 syncManager,err:=entitysync.NewManager(entitysync.ManagerConfig{Mode:entitysync.ModeOnChange,Interval:time.Hour,Transport:syncTransport});if err!=nil{t.Fatal(err)}
 if err=syncManager.Register(player.Sync());err!=nil{t.Fatal(err)}
 t.Cleanup(func(){_ = syncManager.Close(context.Background())})
 business:=nest.NewEngine(nest.NestOptionWithEntitySync(syncManager),nest.NestOptionWithGetter(sceneGetter{player.ID():player,world.ID():world}),nest.NestOptionWithTransactionCommitter(projector),nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers:4,QueueCap:32},nest.WorkerPoolConfig{}))
 if err:=business.Start();err!=nil{t.Fatal(err)};t.Cleanup(func(){_ = business.Shutdown(context.Background())})
 registry:=player_agent.NewProtocolRegistry()
 if err:=player_agent.RegisterProtocol[*pb.AddExpRequest,*pb.AddExpResponse](registry,42,43,pb.UnmarshalAddExpRequest,pb.MarshalAddExpResponse,func(ctx *player_agent.Context,request *pb.AddExpRequest)(*pb.AddExpResponse,error){
  if ctx.PlayerID!=7001{return nil,gateway.ErrUnauthenticated}
  gained,err:=syncsender.NewAddExpSender(business).MultiSync_AddExp(ctx.Context(),player.ID(),world.ID(),request.Amount)
  return &pb.AddExpResponse{LevelsGained:gained},err
 });err!=nil{t.Fatal(err)};if err:=registry.Seal();err!=nil{t.Fatal(err)}
 config:=gateway.DefaultConfig();config.Namespace=fmt.Sprintf("roost.generated.gate.n%d",time.Now().UnixNano())
 gateID,gameID:=gateway.ProcessIdentity{ServerID:11,Incarnation:"gate000000000001"},gateway.ProcessIdentity{ServerID:22,Incarnation:"game000000000001"}
 raw:=func(role string,identity gateway.ProcessIdentity)fnats.RawClient{
  client,err:=driver.NewClient(fnats.DefaultConfig(url),driver.ClientOptions{ReconnectBufferBytes:-1});if err!=nil{t.Fatal(err)};t.Cleanup(client.Close)
  prefix,_:=gateway.InboxPrefix(config.Namespace,role,identity);scoped,err:=client.ForInbox(prefix);if err!=nil{t.Fatal(err)};return scoped
 }
 gameRaw,gateRaw:=raw("game",gameID),raw("gate",gateID)
 auth:=gateway.AuthenticatorFunc(func(context.Context,string,net.Addr)(gateway.Principal,error){return gateway.Principal{PlayerID:7001,ServerID:22,SessionID:"generated"},nil})
 matches:=func(_ context.Context,role string,id gateway.ProcessIdentity)(bool,error){return (role=="game"&&id==gameID)||(role=="gate"&&id==gateID),nil}
 cfg:=viper.New();cfg.Set("player.tcp.enabled",false)
 capabilities:=app.NewRegistry(cfg)
 for _,capability:=range []app.Capability{{Name:app.ModSingletonIncarnation,Value:app.SingletonIncarnation{Sid:gameID.ServerID,Token:gameID.Incarnation}},{Name:app.ModSingletonIdentityChecker,Value:gateChecker{matches}},{Name:mods.ModNatsRaw,Value:gameRaw},{Name:accessplayer.Name,Value:&accessplayer.Runtime{Protocols:registry}}}{if err:=capabilities.Register(capability.Name,capability.Value);err!=nil{t.Fatal(err)}}
 options:=gatewiring.DefaultOptions();options.Config=config;options.Authenticator=auth;options.Ready=func()bool{return true}
 options.OnActive=func(binding gateway.Binding,id uint64)error{if err:=syncManager.OpenSession(entitysync.SessionID(id));err!=nil{return err};return syncManager.Subscribe(entitysync.SessionID(id),player.ID(),entity.SyncProfile{})}
 options.OnClosed=func(_ gateway.Binding,id uint64){syncManager.CloseSession(entitysync.SessionID(id))}
 ingressMod:=playertcp.NewIngressMod(options)
 if err:=ingressMod.Init(cfg);err!=nil{t.Fatal(err)};if err:=ingressMod.Provide(capabilities);err!=nil{t.Fatal(err)}
 ingress,ok:=app.Lookup[*gateway.GameIngress](capabilities,gatewiring.GameIngressName);if !ok{t.Fatal("generated Game ingress not provided")}
 if _,ok:=app.Lookup[*playertcp.Runtime](capabilities,playertcp.Name);!ok{t.Fatal("generated business runtime missing")}

 syncTransport.Game=ingress
 if err=syncManager.Start(context.Background());err!=nil{t.Fatal(err)}
 if err=ingressMod.Start();err!=nil{t.Fatal(err)}
 gate,err:=gateway.NewGate(config,gateID,gateway.GateDependencies{Client:gateRaw,QueueFactory:gatewiring.AsyncQueueFactory,Ready:func()bool{return true},Matches:matches,ResolveGame:func(context.Context,int32)(gateway.ProcessIdentity,error){return gameID,nil}});if err!=nil{t.Fatal(err)}
 if err=gate.Start();err!=nil{t.Fatal(err)}
 tcp:=gateway.DefaultTCPConfig();tcp.Addr="127.0.0.1:0";tcp.MaxPayloadBytes=uint32(config.MaxPayloadBytes)
 server,err:=gateway.NewTCPServer(tcp,gate.Forward,auth);if err!=nil{t.Fatal(err)};if err=server.ConnectHandshake(gate);err!=nil{t.Fatal(err)}
 ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second);defer cancel()
 for _,client:=range []fnats.RawClient{gameRaw,gateRaw}{if err:=client.FlushContext(ctx);err!=nil{t.Fatal(err)}}
 if err=server.Start();err!=nil{t.Fatal(err)}
 t.Cleanup(func(){ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second);defer cancel();if err:=gate.Stop(ctx);err!=nil{t.Error(err)};if err:=server.Stop(ctx);err!=nil{t.Error(err)};if err:=ingressMod.StopWithContext(ctx);err!=nil{t.Error(err)}})
 conn,err:=net.DialTimeout("tcp",server.Addr().String(),time.Second);if err!=nil{t.Fatal(err)};defer conn.Close();_ = conn.SetDeadline(time.Now().Add(5*time.Second))
 if err=wire.Write(conn,[]*wire.Packet{{Seq:1,Payload:[]byte("ticket")}},8192);err!=nil{t.Fatal(err)};if _,err=wire.Read(conn,8192);err!=nil{t.Fatal(err)}
 // Interval 为一小时，下面的增量必须由 handler 完成后的 on_change 唤醒。
 snapshot,err:=wire.Read(conn,config.MaxPayloadBytes);if err!=nil{t.Fatal(err)}
 if (wire.Header{Flags:snapshot.Flags}).Kind()!=wire.PayloadSync||snapshot.MsgID!=501{t.Fatalf("initial Sync packet=%+v",snapshot)}
 full,err:=entitysync.DecodeFrame(snapshot.Payload,frame.DefaultLimits());if err!=nil{t.Fatal(err)}
 if len(full.Objects)!=1||len(full.Objects[0].Components)!=1{t.Fatalf("snapshot=%+v",full)}
 initial,err:=entitysync.DecodeSubjectUpdate(full.Objects[0].Components[0].Data,0);if err!=nil||!initial.Full||initial.SubjectID!=player.ID(){t.Fatalf("initial update=%+v error=%v",initial,err)}
 data,err:=pb.MarshalAddExpRequest(&pb.AddExpRequest{Amount:250});if err!=nil{t.Fatal(err)}
 if err=wire.Write(conn,[]*wire.Packet{{MsgID:42,Seq:2,Payload:data}},config.MaxPayloadBytes);err!=nil{t.Fatal(err)}
 var gotResponse,gotDelta bool
 for range 2{
  response,err:=wire.Read(conn,config.MaxPayloadBytes);if err!=nil{t.Fatal(err)}
  if (wire.Header{Flags:response.Flags}).Kind()==wire.PayloadSync{
   delta,err:=entitysync.DecodeFrame(response.Payload,frame.DefaultLimits());if err!=nil{t.Fatal(err)}
   if delta.Epoch!=full.Epoch||delta.Tick<=full.Tick||len(delta.Objects)!=1||len(delta.Objects[0].Components)!=1{t.Fatalf("delta frame=%+v",delta)}
   update,err:=entitysync.DecodeSubjectUpdate(delta.Objects[0].Components[0].Data,0);if err!=nil||update.Full||update.SubjectID!=player.ID()||update.BaseVersion!=initial.Version||update.Version<=initial.Version||update.Payload.Len()==0{t.Fatalf("delta=%+v err=%v",update,err)}
   gotDelta=true
  }else{decoded,err:=pb.UnmarshalAddExpResponse(response.Payload);if err!=nil||decoded.Code!=0||decoded.LevelsGained==0||response.MsgID!=43||response.Seq!=2{t.Fatalf("response=%+v packet=%+v err=%v",decoded,response,err)};gotResponse=true}
 }
 if !gotResponse||!gotDelta{t.Fatalf("response=%v delta=%v",gotResponse,gotDelta)}
 if err=projector.Flush(ctx);err!=nil{t.Fatal(err)}
 store.mu.Lock();defer store.mu.Unlock()
 if len(store.records)!=1{t.Fatalf("unique transactions=%d",len(store.records))}
 for _,record:=range store.records{ids:=make(map[int64]bool);for _,mutation:=range record.Mutations{ids[mutation.Key.ID]=true};if len(ids)!=2{t.Fatalf("two-entity transaction=%+v",record)}}
 if stats:=wal.Stats();stats.Appended!=1{t.Fatalf("WAL=%+v",stats)}
}
`

func TestGateGeneratedBusinessConsumer(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and executes formal generated business consumer")
	}
	if os.Getenv("ROOST_DATAENGINE_IT_NATS_URL") == "" {
		t.Skip("private NATS URL not configured")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := copyOfNewProject(t, "game-demo")
	modPath := filepath.Join(root, "go.mod")
	mod, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatal(err)
	}
	mod = regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`).ReplaceAll(mod, nil)
	mod = append(mod, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
	if err = os.WriteFile(modPath, mod, 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "internal/service/game/gate_consumer_test.go"), []byte(gateConsumerTest), 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-mod=mod", "-race", "-count=1", "-v", "-run", "^TestGeneratedGateNestDAOAndWAL$", "./internal/service/game")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated Gate business: %v\n%s", err, output)
	}
	t.Log(string(output))
}
