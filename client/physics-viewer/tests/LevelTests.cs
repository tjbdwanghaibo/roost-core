using System;
using System.Collections.Generic;
using System.IO;
using System.Numerics;
using System.Text;
using System.Text.Json;
using Roost.PhysicsViewer;

// RP3 / 传送标记 / 流程视图 / 关卡标记：金样与 Go 共用 packed.json，断言的值与 Go 侧同名用例一致。
static class LevelTests
{
 static void Check(bool value,string reason){if(!value)throw new Exception(reason);}
 // 必须以 InvalidDataException 拒绝；越界之类的其他异常说明解码器自己出了错。
 static void Rejects(Action action,string reason)
 {
  try{action();}catch(InvalidDataException){return;}
  throw new Exception("accepted: "+reason);
 }
 static bool Near(float a,float b)=>Math.Abs(a-b)<.0001f;
 static PredictedState Step(PredictedState s,PredictedInput input,float dt)
 {
  if(input.Jump)s.Velocity.Y+=7;
  s.Velocity+=input.Acceleration*dt;s.Position+=s.Velocity*dt;return s;
 }
 public static void Run()
 {
  using var doc=JsonDocument.Parse(File.ReadAllText("client/physics-viewer/tests/packed.json"));
  byte[] Golden(string name)=>Convert.FromBase64String(doc.RootElement.GetProperty(name).GetString());
  RoomFrames(Golden("RoomFull"),Golden("RoomTeleport"));
  Playback();
  Prediction();
  LevelView(Golden("LevelView"));
  Markers(Encoding.UTF8.GetString(Golden("Level")));
 }
 // 与 gameplay/physics TestRoomTeleportGoldenSharedWithCSharp 断言同样的值。
 static void RoomFrames(byte[] full,byte[] teleport)
 {
  var replica=new ChunkReplica();
  Check(replica.Apply(full),"RoomFull is a full frame");
  var one=replica.Bodies[1];
  Check(replica.Bodies.Count==2&&replica.Tick==1&&replica.InputSequence==0&&!replica.InputActive,"full frame header");
  Check(one.Position.x==3&&one.Position.y==1&&one.Position.z==-2&&one.Velocity.z==1.5f&&!one.Teleported&&!replica.Bodies[2].Teleported,"full frame body");
  Check(!replica.Apply(teleport),"RoomTeleport is a delta frame");
  one=replica.Bodies[1];
  Check(replica.Tick==2&&replica.InputSequence==7&&replica.InputGeneration==3&&replica.InputActive,"teleport frame acknowledgement");
  Check(one.Teleported&&one.SampleTick==2&&one.Position.x==2&&one.Position.y==.5f&&one.Position.z==3,"teleported body carries the flag and the new position");
  Check(one.Velocity.x==0&&one.Velocity.y==0&&one.Velocity.z==0,"teleported body is at rest");
  // 没有随这一帧更新的对象保持原样：标记没有，采样帧号也还是旧的，调用方据此只处理这一帧的传送。
  Check(!replica.Bodies[2].Teleported&&replica.Bodies[2].SampleTick==1,"untouched body keeps its sample");
  // RP2 的帧在帧头就拒绝，不等第一次传送。
  byte[] old=(byte[])full.Clone();old[2]=2;
  Rejects(()=>new ChunkReplica().Apply(old),"RP2 frame");
  byte[] next=(byte[])full.Clone();next[2]=4;
  Rejects(()=>new ChunkReplica().Apply(next),"RP4 frame");
  // 状态字节是每个对象 34 字节量化状态的最后一个字节；整帧里刚体 2 排在最后。
  byte[] flag=(byte[])full.Clone();flag[flag.Length-1]=16;
  Rejects(()=>new ChunkReplica().Apply(flag),"unknown status bit in a frame");
  flag[flag.Length-1]=8;
  var marked=new ChunkReplica();marked.Apply(flag);
  Check(marked.Bodies[2].Teleported&&!marked.Bodies[1].Teleported,"status bit 3 in a full frame");
  for(int i=0;i<teleport.Length;i++)
  {
   var partial=new ChunkReplica();partial.Apply(full);
   byte[] cut=teleport.AsSpan(0,i).ToArray();
   bool rejected=false;
   try{partial.Apply(cut);}catch(InvalidDataException){rejected=true;}catch(EndOfStreamException){rejected=true;}
   Check(rejected&&partial.Tick==1&&!partial.Bodies[1].Teleported,"truncated teleport frame must not change the replica");
  }
  Console.WriteLine("PASS room frame: RP3 Go/C# golden, teleport flag, RP2 refused, truncation leaves the replica intact");
 }
 static void Playback()
 {
  DisplayPose At(float x)=>new DisplayPose{Position=Vector3.UnitX*x,Rotation=Quaternion.Identity};
  // 对照：不带标记时，同样的三个样本会从旧位置朝新位置插值。
  var smooth=new SparsePosePlayback();
  smooth.Sample(At(0),1,0);smooth.Sample(At(10),11,.2);smooth.Sample(At(100),12,.22);
  float between=smooth.Read(.23).Position.X;
  Check(between>0&&between<100,"control: an unmarked jump is interpolated");
  var playback=new SparsePosePlayback();
  playback.Sample(At(0),1,0);playback.Sample(At(10),11,.2);
  playback.Sample(At(100),12,.22,true);
  Check(playback.Read(.22).Position.X==100&&playback.Read(.23).Position.X==100&&playback.Read(5).Position.X==100,"teleported pose is shown at once and held");
  playback.Sample(At(102),22,.42);
  for(double now=.42;now<.8;now+=.01)
  {
   float x=playback.Read(now).Position.X;
   Check(x>=100&&x<=102,"after a teleport playback continues from the new position");
  }
  Check(playback.Read(.8).Position.X==102,"playback reaches the next sample");
  // 旧帧号的样本仍然忽略，即使带标记。
  playback.Sample(At(-50),5,.9,true);
  Check(playback.Read(.9).Position.X==102,"a stale marked sample is ignored");
  Console.WriteLine("PASS playback discontinuity: teleport placed at once, buffer dropped, stale sample ignored");
 }
 static void Prediction()
 {
  var spawn=new PredictedState{Position=Vector3.UnitX*50};
  // 对照：不带标记时，已确认的那条输入会被继续外推。
  var ordinary=new InputPrediction();
  ordinary.Record(new PredictedInput{Sequence=1,At=0,Acceleration=Vector3.UnitX*10});
  ordinary.Reconcile(1,true,spawn,.05);
  Check(ordinary.Predict(.1,Step).Position.X>50,"control: an acknowledged input is extrapolated");
  var p=new InputPrediction();
  p.Record(new PredictedInput{Sequence=1,At=0,Acceleration=Vector3.UnitX*10});
  p.Reconcile(1,true,spawn,.05,true);
  Check(p.Predict(.1,Step).Position==spawn.Position&&p.Predict(.25,Step).Position==spawn.Position,"a teleported frame is not extrapolated with the old input");
  // 传送之后按下的输入照常从新位置重放。
  p.Record(new PredictedInput{Sequence=2,At=.1,Acceleration=Vector3.UnitX*10});
  float moved=p.Predict(.14,Step).Position.X-50;
  Check(moved>0&&moved<.05f,"inputs after the teleport replay from the new position");
  // 下一帧不带标记：恢复外推。
  p.Reconcile(2,true,spawn,.15);
  Check(p.Predict(.2,Step).Position.X>50,"extrapolation resumes on the next frame");
  // 传送帧确认到 2：1、2 的历史（含 2 里的跳跃）删去，只重放 3。
  var q=new InputPrediction();
  q.Record(new PredictedInput{Sequence=1,At=0,Acceleration=Vector3.UnitX*10});
  q.Record(new PredictedInput{Sequence=2,At=.05,Acceleration=Vector3.UnitX*10,Jump=true});
  q.Record(new PredictedInput{Sequence=3,At=.1,Acceleration=Vector3.UnitX*10});
  q.Reconcile(2,true,spawn,.12,true);
  var replayed=q.Predict(.14,Step);
  Check(replayed.Position.Y==0&&replayed.Velocity.Y==0,"a jump pressed before the teleport is not replayed at the spawn");
  Check(Near(replayed.Velocity.X,.4f)&&replayed.Position.X>50&&replayed.Position.X<50.05f,"only the input after the acknowledged one is replayed");
  Console.WriteLine("PASS prediction discontinuity: no extrapolation of the old input, older history dropped, later inputs replayed");
 }
 // 手工拼 Protobuf 字节，用来构造坏包。
 static byte[] Varint(ulong v){var o=new List<byte>();while(v>=128){o.Add((byte)(v|128));v>>=7;}o.Add((byte)v);return o.ToArray();}
 static byte[] Join(params byte[][] parts){var o=new List<byte>();foreach(var p in parts)o.AddRange(p);return o.ToArray();}
 static byte[] Num(int field,ulong v)=>Join(Varint((ulong)(field<<3)),Varint(v));
 static byte[] Sub(int field,byte[] body)=>Join(Varint((ulong)(field<<3|2)),Varint((ulong)body.Length),body);
 static byte[] Str(int field,string text)=>Sub(field,Encoding.UTF8.GetBytes(text));
 // 与 service/physicsroom TestLevelViewGoldenSharedWithCSharp、TestDecodeLevelViewRejectsMalformedInput 对应。
 static void LevelView(byte[] golden)
 {
  var view=RoomLevelView.Decode(golden);
  Check(view.Revision==300&&view.Phase=="finishing"&&view.Result=="clear","level view header");
  Check(view.Stages.Count==2&&view.Stages[0].Id=="finale"&&view.Stages[0].Generation==2&&view.Stages[1].Id=="run"&&view.Stages[1].Generation==1,"level view stages");
  Check(view.Variables.Count==4,"level view variable count");
  var laps=view.Variables[0];var open=view.Variables[1];var title=view.Variables[2];var zero=view.Variables[3];
  Check(laps.Key=="laps"&&laps.Kind==2&&laps.Int==-3&&laps.ToString()=="-3","negative integer variable");
  Check(open.Key=="open"&&open.Kind==3&&!open.Flag&&open.ToString()=="false","false flag is still a flag");
  Check(title.Key=="title"&&title.Kind==4&&title.Text=="第一关"&&title.ToString()=="第一关","text variable");
  Check(zero.Key=="zero"&&zero.Kind==2&&zero.Int==0&&zero.ToString()=="0","zero integer is still an integer");
  // 同一条连接上的版本次序。
  var current=RoomLevelView.Advance(null,golden);
  Check(current.Revision==300,"first view accepted");
  Check(ReferenceEquals(RoomLevelView.Advance(current,golden),current),"a repeated revision keeps the current view");
  byte[] phase=Str(2,"running");
  var newer=RoomLevelView.Advance(current,Join(Num(1,301),phase));
  Check(newer.Revision==301&&newer.Phase=="running"&&newer.Result==""&&newer.Stages.Count==0&&newer.Variables.Count==0,"a newer revision replaces the view");
  Rejects(()=>RoomLevelView.Advance(current,Join(Num(1,299),phase)),"revision going backwards");
  Check(RoomLevelView.Decode(phase).Revision==0,"revision may be absent");
  var bad=new Dictionary<string,byte[]>{
   {"empty",new byte[0]},
   {"no phase",Num(1,5)},
   {"empty phase",Join(Num(1,5),Str(2,""))},
   {"unknown field",Join(phase,Num(6,1))},
   {"unknown bytes field",Join(phase,Str(6,"x"))},
   {"unknown nested message",Join(phase,Sub(7,Str(1,"a")))},
   {"field number beyond int",Join(phase,Varint(ulong.MaxValue&~7UL),Varint(1))},
   {"revision twice",Join(Num(1,1),Num(1,2),phase)},
   {"phase twice",Join(phase,phase)},
   {"result twice",Join(phase,Str(3,"a"),Str(3,"b"))},
   {"revision as bytes",Join(Str(1,"x"),phase)},
   {"phase as varint",Num(2,1)},
   {"fixed64 wire type",Join(phase,new byte[]{1<<3|1,0,0,0,0,0,0,0,0})},
   {"truncated phase",new byte[]{2<<3|2,5,(byte)'r'}},
   {"truncated revision",new byte[]{1<<3,0x80}},
   {"varint overflow",Join(new byte[]{1<<3,255,255,255,255,255,255,255,255,255,2},phase)},
   {"invalid UTF-8",Join(Sub(2,new byte[]{0xff,0xfe}))},
   {"stage without id",Join(phase,Sub(4,Num(2,1)))},
   {"stage with empty id",Join(phase,Sub(4,Str(1,"")))},
   {"stage repeated",Join(phase,Sub(4,Str(1,"a")),Sub(4,Str(1,"a")))},
   {"stage id twice",Join(phase,Sub(4,Join(Str(1,"a"),Str(1,"b"))))},
   {"stage generation twice",Join(phase,Sub(4,Join(Str(1,"a"),Num(2,1),Num(2,2))))},
   {"stage unknown field",Join(phase,Sub(4,Join(Str(1,"a"),Num(3,1))))},
   {"stage id as varint",Join(phase,Sub(4,Num(1,1)))},
   {"stage truncated",Join(phase,new byte[]{4<<3|2,3,1<<3|2,5,(byte)'a'})},
   {"variable without value",Join(phase,Sub(5,Str(1,"k")))},
   {"variable without key",Join(phase,Sub(5,Num(2,1)))},
   {"variable with two values",Join(phase,Sub(5,Join(Str(1,"k"),Num(2,1),Num(3,1))))},
   {"variable value twice",Join(phase,Sub(5,Join(Str(1,"k"),Num(2,1),Num(2,2))))},
   {"variable repeated",Join(phase,Sub(5,Join(Str(1,"k"),Num(2,1))),Sub(5,Join(Str(1,"k"),Num(3,1))))},
   {"flag out of range",Join(phase,Sub(5,Join(Str(1,"k"),Num(3,2))))},
   {"variable unknown field",Join(phase,Sub(5,Join(Str(1,"k"),Num(2,1),Num(5,1))))},
   {"integer as bytes",Join(phase,Sub(5,Join(Str(1,"k"),Str(2,"1"))))},
   {"text as varint",Join(phase,Sub(5,Join(Str(1,"k"),Num(4,1))))},
  };
  foreach(var pair in bad)Rejects(()=>RoomLevelView.Decode(pair.Value),"level view: "+pair.Key);
  // 截断：前缀要么是一条较短的合法消息，要么以 InvalidDataException 拒绝，不能抛别的。
  int refused=0;
  for(int i=0;i<golden.Length;i++)
  {
   try{RoomLevelView.Decode(golden.AsSpan(0,i).ToArray());}catch(InvalidDataException){refused++;}
  }
  Check(refused>golden.Length/2,"most truncations of the golden are refused");
  // 上限 16KiB：恰好可以，多一个字节拒绝。
  byte[] head=Join(phase,new byte[]{5<<3|2});
  int room=RoomLevelView.MaxBytes-head.Length;
  byte[] exact=null;
  for(int text=room-16;text<room&&exact==null;text++)
  {
   byte[] candidate=Join(phase,Sub(5,Join(Str(1,"k"),Str(4,new string('x',text)))));
   if(candidate.Length==RoomLevelView.MaxBytes)exact=candidate;
  }
  Check(exact!=null&&RoomLevelView.Decode(exact).Variables[0].Text.Length>16000,"a view of exactly the limit is accepted");
  Rejects(()=>RoomLevelView.Decode(Join(exact,Num(1,1))),"level view over the byte limit");
  Console.WriteLine("PASS level view: Go/C# golden, revision order, "+bad.Count+" malformed inputs, truncation and byte limit");
 }
 // 与 gameplay/physics TestLevelMarkersGoldenSharedWithCSharp 断言同样的值。
 static void Markers(string level)
 {
  LevelMarkers.Parse(level,out var regions,out var spawns,out string fallback);
  Check(regions.Length==2&&spawns.Length==2&&fallback=="start","marker counts");
  Check(regions[0].Id=="kill"&&regions[0].Center==new Vector3(0,-6,0)&&regions[0].HalfExtents==new Vector3(60,2,60)&&regions[0].Rotation==Quaternion.Identity,"zero rotation reads as identity");
  Check(regions[1].Id=="goal"&&regions[1].Center==new Vector3(8,1,8)&&regions[1].HalfExtents==new Vector3(1,1.5f,2),"rotated region box");
  Check(regions[1].Rotation.X==0&&Near(regions[1].Rotation.Y,.38268343f)&&regions[1].Rotation.Z==0&&Near(regions[1].Rotation.W,.92387953f),"rotated region rotation");
  Check(spawns[0].Id=="start"&&spawns[0].Positions.Length==2&&spawns[0].Positions[0]==new Vector3(0,.6f,-3)&&spawns[0].Positions[1]==new Vector3(1,.6f,-3)&&spawns[0].Rotation==Quaternion.Identity,"spawn with two positions");
  Check(spawns[1].Id=="mid"&&spawns[1].Positions.Length==1&&spawns[1].Positions[0]==new Vector3(4,.6f,0)&&spawns[1].Rotation==new Quaternion(0,1,0,0),"spawn rotation");
  // 没有区域与出生点的关卡（Go 把空切片写成 null）；字符串里的转义要读对，才不会把后面的内容读错位。
  LevelMarkers.Parse("{\"Name\":\"a\\u003cb\\\"\\\\ ] }\",\"Regions\":null,\"Spawns\":[],\"Flow\":{\"x\":[1,-2.5e3,true,false,null,{}]}}",out regions,out spawns,out fallback);
  Check(regions.Length==0&&spawns.Length==0&&fallback=="","level without markers");
  LevelMarkers.Parse(" { } ",out regions,out spawns,out fallback);
  Check(regions.Length==0&&spawns.Length==0,"empty document");
  LevelMarkers.Parse("{\"Flow\":"+new string('[',30)+new string(']',30)+"}",out regions,out spawns,out fallback);
  Check(regions.Length==0,"control: nesting within the limit is accepted");
  string region="{\"ID\":\"a\",\"Center\":[0,0,0],\"HalfExtents\":[1,1,1],\"Rotation\":[0,0,0,0]}";
  string spawn="{\"ID\":\"s\",\"Positions\":[[0,0,0]],\"Rotation\":[0,0,0,1]}";
  LevelMarkers.Parse("{\"Regions\":["+region+"],\"Spawns\":["+spawn+"]}",out regions,out spawns,out fallback);
  Check(regions.Length==1&&spawns.Length==1,"control: the minimal markers are accepted");
  var bad=new Dictionary<string,string>{
   {"not an object","[]"},
   {"trailing content","{} x"},
   {"regions not an array","{\"Regions\":{}}"},
   {"region not an object","{\"Regions\":[1]}"},
   {"region without id","{\"Regions\":["+region.Replace("\"ID\":\"a\",","")+"]}"},
   {"region with empty id","{\"Regions\":["+region.Replace("\"a\"","\"\"")+"]}"},
   {"region center of two numbers","{\"Regions\":["+region.Replace("\"Center\":[0,0,0]","\"Center\":[0,0]")+"]}"},
   {"region without size","{\"Regions\":["+region.Replace("\"HalfExtents\":[1,1,1],","")+"]}"},
   {"region with zero size","{\"Regions\":["+region.Replace("[1,1,1]","[1,0,1]")+"]}"},
   {"region rotation not unit","{\"Regions\":["+region.Replace("[0,0,0,0]","[0,0,0,2]")+"]}"},
   {"region number as string","{\"Regions\":["+region.Replace("[1,1,1]","[1,\"1\",1]")+"]}"},
   {"region number out of range","{\"Regions\":["+region.Replace("[1,1,1]","[1,1e9,1]")+"]}"},
   {"spawn without positions","{\"Spawns\":["+spawn.Replace("[[0,0,0]]","[]")+"]}"},
   {"spawn position of four numbers","{\"Spawns\":["+spawn.Replace("[[0,0,0]]","[[0,0,0,0]]")+"]}"},
   {"spawn positions flat","{\"Spawns\":["+spawn.Replace("[[0,0,0]]","[0,0,0]")+"]}"},
   {"duplicate key","{\"Regions\":[],\"Regions\":[]}"},
   {"unterminated string","{\"Name\":\"abc}"},
   {"bad escape","{\"Name\":\"\\x\"}"},
   {"missing colon","{\"Name\" 1}"},
   {"missing comma","{\"Regions\":[] \"Spawns\":[]}"},
   {"bare word","{\"Name\":nil}"},
   {"too many regions","{\"Regions\":["+string.Join(",",System.Linq.Enumerable.Repeat(region,65))+"]}"},
   {"too many positions","{\"Spawns\":["+spawn.Replace("[[0,0,0]]","["+string.Join(",",System.Linq.Enumerable.Repeat("[0,0,0]",21))+"]")+"]}"},
   {"nesting too deep","{\"Flow\":"+new string('[',40)+new string(']',40)+"}"},
  };
  foreach(var pair in bad)Rejects(()=>LevelMarkers.Parse(pair.Value,out _,out _,out _),"level markers: "+pair.Key);
  for(int i=0;i<level.Length;i++)
  {
   string cut=level.Substring(0,i);
   Rejects(()=>LevelMarkers.Parse(cut,out _,out _,out _),"truncated level document at "+i);
  }
  Console.WriteLine("PASS level markers: Go/C# golden, level without markers, "+bad.Count+" malformed documents and truncation");
 }
}
