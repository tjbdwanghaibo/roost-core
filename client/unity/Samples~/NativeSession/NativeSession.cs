using System;
using Google.Protobuf;
using Google.Protobuf.WellKnownTypes;
using Roost.Native;
using Roost.Client;
using Roost.Dao.Sync;
using UnityEngine;
namespace Roost.Unity.Samples
{
// Sample routes belong to the SDK loopback fixture. Real games use generated
// message IDs.
[RequireComponent(typeof(RoostNativeDriver))]
public sealed class NativeSession : MonoBehaviour
{
    public string Address = "127.0.0.1";
    public int Port;
    public string Ticket = "ticket";
    private NativeGameClient game;
    private NativeReconnect retry;
    private NativeLockstep match;
    private int ready, responses, pushes, frames, restored, recovered, full, delta, removed;
    private bool acknowledged, automated, finished;
    private float started;
    private string status = "Disconnected";
    private void Start()
    {
        var args = Environment.GetCommandLineArgs();
        int capacity = Array.IndexOf(args, "-roostCapacity"), report = Array.IndexOf(args, "-roostReport");
        if (capacity >= 0 && report >= 0)
        {
            try
            {
                CapacityRun.Run(args[capacity + 1], args[report + 1]);
                Application.Quit(0);
            }
            catch (Exception error)
            {
                Debug.LogException(error);
                Application.Quit(1);
            }
            return;
        }

        for (int i = 0; i < args.Length; i++)
            if (args[i] == "-roostPort" && i + 1 < args.Length)
            {
                Port = int.Parse(args[i + 1]);
                automated = true;
            }
        if (Port > 0)
            Connect();
    }
    public void Connect()
    {
        Close();
        ready = responses = pushes = frames = restored = recovered = full = delta = removed = 0;
        acknowledged = finished = false;
        started = Time.realtimeSinceStartup;
        game = new NativeGameClient();
        game.Entities.Register<SyncHeroDao>("hero", "default", 2, 1,
                                            (update, previous) =>
                                                SyncHeroDao.Parser.ParseFrom(update.Payload).Apply(previous));
        game.EntityChanged += change =>
        {
            if (change.Kind == EntityChangeKind.Remove)
            {
                removed++;
                return;
            }
            var value = (SyncHeroDao)change.Current.Value;
            if (value.EntityId != 9007199254740993L || value.Exp.Value != (1L << 60))
                throw new Exception("DAO identity/precision mismatch");
            if (change.Kind == EntityChangeKind.Full)
            {
                if (value.Level.Value != 7)
                    throw new Exception("Full mismatch");
                full++;
            }
            else
            {
                if (value.Level.Value != 0 || value.Items.Value.Count != 0)
                    throw new Exception("Delta zero/empty mismatch");
                delta++;
            }
        };
        retry = new NativeReconnect(
            game,
            _ => new NativeReconnect.Ticket { Address = Address, Port = (ushort)Port, Value = Ticket }, 2, 0.1, 0.2);
        retry.Reconnected += () => recovered++;
        game.Subscribe(9, b => Int32Value.Parser.ParseFrom(b),
                       m =>
                       {
                           if (m.Value != 3)
                               throw new Exception("PB push mismatch");
                           pushes++;
                       });
        game.Error += e =>
        {
            status = e.Message;
            Debug.Log("Roost: " + e.Message);
        };
        game.Ready += () =>
        {
            ready++;
            status = "Ready";
            match?.Dispose();
            match = new NativeLockstep(game, 50001, 50002,
                                       (frame, inputs) =>
                                       { frames++; },
                                       snapshot =>
                                       {
                                           if (System.Text.Encoding.UTF8.GetString(snapshot.Data) !=
                                               "roost-snapshot-golden-24")
                                               throw new Exception("Snapshot mismatch");
                                           restored++;
                                           return 9223372036854780468UL;
                                       });
            game.Request(42, 43, new Int32Value { Value = 1 }.ToByteArray(), b => Int32Value.Parser.ParseFrom(b),
                         m =>
                         {
                             if (m.Value != 2)
                                 throw new Exception("PB reply mismatch");
                             responses++;
                         },
                         e =>
                         { status = e.Message; });
            match.Submit(1, new byte[] { 8, 42 });
        };
        GetComponent<RoostNativeDriver>().Attach(game, retry);
        game.Connect(Address, (ushort)Port, Ticket);
    }
    private void Update()
    {
        if (game == null || finished)
            return;
        try
        {
            if (!acknowledged && responses == 1 && pushes == 1 && frames == 2 && restored == 1 && full == 1 &&
                delta == 1 && removed == 1)
            {
                acknowledged = true;
                game.Notify(99, 0, Array.Empty<byte>(),
                            _ =>
                            {});
            }
            if (ready == 2 && recovered == 1 && responses == 2 && pushes == 2 && frames == 4 && restored == 2 &&
                full == 2 && delta == 2 && removed == 2)
            {
                finished = true;
                status = "PASS native Unity PB / EntitySync / Lockstep / snapshot / " + "reconnect";
                Debug.Log(status);
                Close();
                if (automated)
                    Application.Quit(0);
            }
            if (automated && Time.realtimeSinceStartup - started > 20)
            {
                Debug.LogError("Roost acceptance timeout: " + status);
                Close();
                Application.Quit(1);
            }
        }
        catch (Exception e)
        {
            Debug.LogException(e);
            Close();
            if (automated)
                Application.Quit(1);
        }
    }
    private void OnGUI()
    {
        GUILayout.Label("Roost Native TCP / PB / Lockstep");
        GUILayout.Label(status);
        GUILayout.Label(
            $"Connections {ready}, replies {responses}, pushes {pushes}, frames {frames}, snapshots {restored}, Sync {full}/{delta}/{removed}");
        if (GUILayout.Button("Connect"))
            Connect();
        if (GUILayout.Button("Disconnect"))
            Close();
    }
    private void OnApplicationPause(bool paused)
    {
        if (paused)
        {
            Close();
            status = "Paused; fetch a new ticket before connecting";
        }
    }
    private void Close()
    {
        match?.Dispose();
        match = null;
        GetComponent<RoostNativeDriver>()?.Detach();
        retry?.Dispose();
        retry = null;
        game?.Dispose();
        game = null;
    }
    private void OnDestroy()
    {
        Close();
    }
}
}
