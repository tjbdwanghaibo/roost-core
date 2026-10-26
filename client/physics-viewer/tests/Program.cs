using System;
using System.Numerics;
using System.IO;
using System.Text.Json;
using Roost.PhysicsViewer;

static class Program
{
 static void Check(bool value,string reason){if(!value)throw new Exception(reason);}
 static int jumps;
 static PredictedState Step(PredictedState s,PredictedInput input,float dt)
 {
  if(input.Jump){jumps++;s.Velocity.Y+=7;}
  s.Velocity+=input.Acceleration*dt;s.Position+=s.Velocity*dt;return s;
 }
 static void Packed()
 {
  using var doc=JsonDocument.Parse(File.ReadAllText("client/physics-viewer/tests/packed.json"));
  byte[] full=Convert.FromBase64String(doc.RootElement.GetProperty("Full").GetString());
  byte[] delta=Convert.FromBase64String(doc.RootElement.GetProperty("Delta").GetString());
  var baseline=PackedBodyCodec.Apply(null,full);var next=PackedBodyCodec.Apply(baseline,delta);
  var state=PackedBodyCodec.Decode(next,0,-1);
  Check(Math.Abs(state.Position.X-12.35f)<.00001f&&Math.Abs(state.Position.Z+.01f)<.00001f,"Go/C# position golden");
  Check(state.Rotation.W==1&&state.Velocity.Y==-10.5f,"Go/C# rotation/velocity golden");
  Check(PackedBodyCodec.Decode(baseline,0,-1).Position.X==12.34f,"patch must not mutate old baseline");
  for(int i=0;i<full.Length;i++) {
   bool rejected=false;try{PackedBodyCodec.Apply(null,full.AsSpan(0,i).ToArray());}catch(InvalidDataException){rejected=true;}
   Check(rejected,"truncated full accepted");
  }
  bool missing=false;try{PackedBodyCodec.Apply(null,delta);}catch(InvalidDataException){missing=true;}Check(missing,"partial without baseline");
  byte[] invalid=(byte[])full.Clone();invalid[0]=32;bool bad=false;try{PackedBodyCodec.Apply(baseline,invalid);}catch(InvalidDataException){bad=true;}Check(bad,"unknown mask");
  byte[] flags=(byte[])baseline.Clone();flags[33]=7;
  var profiled=PackedBodyCodec.Decode(flags,0,-1);
  Check(profiled.Sleeping&&profiled.DisplayOnly&&profiled.Trajectory,"RP2 profile flags");
  foreach(byte invalidFlag in new byte[]{4,8}) {flags[33]=invalidFlag;bool rejected=false;try{PackedBodyCodec.Decode(flags,0,-1);}catch(InvalidDataException){rejected=true;}Check(rejected,"invalid profile flags");}
  Console.WriteLine("PASS packed body: Go/C# golden, delta, truncation, missing baseline and immutable history");
 }
 static void Playback()
 {
  var playback=new SparsePosePlayback();
  var start=new DisplayPose{Position=Vector3.Zero,Rotation=Quaternion.Identity};
  playback.Sample(start,1,0);
  var next=new DisplayPose{Position=Vector3.UnitX*10,Rotation=Quaternion.Identity};
  playback.Sample(next,11,.2);
  Check(Math.Abs(playback.Read(.3).Position.X-5)<.0001,"sparse interpolation interval");
  playback.Sample(next,11,.3);
  Check(Math.Abs(playback.Read(.35).Position.X-7.5f)<.0001,"unchanged global frame reset interpolation");
  Check(playback.Read(10).Position.X==10,"stall must freeze at authority");
  var motion=new MotionDescription{Translation=Vector3.UnitY*2,PeriodSeconds=4,Axis=Vector3.UnitY,AngularSpeed=1};
  var origin=new DisplayPose{Position=new Vector3(2,1,-3),Rotation=Quaternion.Identity};
  using var doc=JsonDocument.Parse(File.ReadAllText("client/physics-viewer/tests/motion.json"));
  foreach(var sample in doc.RootElement.GetProperty("Samples").EnumerateArray()) {
   ulong tick=sample.GetProperty("Tick").GetUInt64();
   var p=sample.GetProperty("Position");var q=sample.GetProperty("Rotation");
   var expected=new DisplayPose{Position=new Vector3(p[0].GetSingle(),p[1].GetSingle(),p[2].GetSingle()),Rotation=new Quaternion(q[0].GetSingle(),q[1].GetSingle(),q[2].GetSingle(),q[3].GetSingle())};
   var actual=MotionPlayback.Evaluate(motion,origin,0,tick);
   Check(Vector3.Distance(actual.Position,expected.Position)<.00001,"Go/C# trajectory position");
   Check(Math.Abs(Quaternion.Dot(actual.Rotation,expected.Rotation))>.999999,"Go/C# trajectory rotation");
   var calibrated=MotionPlayback.Evaluate(motion,expected,tick,tick+10);
   var reference=MotionPlayback.Evaluate(motion,origin,0,tick+10);
   Check(Vector3.Distance(calibrated.Position,reference.Position)<.00001,"calibration phase mismatch");
   Check(Math.Abs(Quaternion.Dot(calibrated.Rotation,reference.Rotation))>.999999,"calibration rotation mismatch");
  }
  Console.WriteLine("PASS playback: sparse samples, stalled stream, Go/C# trajectory phase and calibration");
 }
 static void Main()
 {
  Packed();
  Playback();
  var p=new InputPrediction();
  p.Record(new PredictedInput{Sequence=1,At=0,Acceleration=Vector3.UnitX*10,Jump=true});
  p.Reconcile(0,false,new PredictedState(),.01);
  Check(p.Predict(.05,Step).Position.X>0,"pending input must predict");
  Check(jumps==1,"one jump per replay, not every substep");
  jumps=0;
  p.Reconcile(1,true,new PredictedState{Position=Vector3.UnitX*3},.06);
  Check(p.Predict(.1,Step).Position.X>3,"rebase at authority");
  Check(jumps==0,"acknowledged jump must not replay");
  p.Reconcile(1,false,new PredictedState{Position=Vector3.UnitX*4},.12);
  Check(p.Predict(.16,Step).Position.X==4,"expired input must stop acceleration");
  Check(p.Predict(1,Step).Position.X==4,"stale snapshots must stop prediction");
  bool bad=false;try{p.Reconcile(2,true,new PredictedState(),1);}catch(ArgumentException){bad=true;}
  Check(bad,"cannot acknowledge unsent input");
  for(ulong i=2;i<40;i++)p.Record(new PredictedInput{Sequence=i,At=i*.05,Acceleration=Vector3.UnitX});
  Check(p.Held,"history overflow must hold");
  p.Reconcile(35,false,new PredictedState(),2);
  Check(!p.Held,"acknowledging dropped history recovers while newer inputs remain");
  p.Reconcile(39,false,new PredictedState(),2);
  Check(!p.Held,"confirmed baseline recovers overflow");
  p.Record(new PredictedInput{Sequence=40,At=2,Acceleration=Vector3.UnitX,Jump=true});
  p.Reconcile(39,false,new PredictedState(),3);
  Check(p.Predict(3.01,Step).Position==Vector3.Zero,"late input must expire independently of fresh snapshot");
  Console.WriteLine("PASS prediction: rebase, sampled ACK, single jump, expiry, bounded history and recovery");
 }
}
