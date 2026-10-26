using System;
using System.Numerics;
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
 static void Main()
 {
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
