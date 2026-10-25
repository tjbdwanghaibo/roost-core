using System;
using System.IO;
using UnityEditor;
using UnityEditor.SceneManagement;
using UnityEngine;
using Roost.PhysicsViewer;

[InitializeOnLoad]
public static class ViewerProject
{
    const string Verification = "RoostPhysicsVerification";
    public static void OpenInteractive()
    {
        EditorSceneManager.OpenScene("Assets/Scenes/PhysicsComparison.unity");
        EditorApplication.isPlaying = true;
    }
    public static void OpenCrowdInteractive()
    {
        if (!File.Exists("Assets/Scenes/Crowd.unity")) CreateCrowdScene();
        CreateRoomScene();
        EditorSceneManager.OpenScene("Assets/Scenes/Crowd.unity");
        EditorApplication.isPlaying = true;
    }
    static ViewerProject()
    {
        EditorApplication.update += CheckVerification;
    }
    public static void CreateScene()
    {
        EditorSceneManager.NewScene(NewSceneSetup.EmptyScene, NewSceneMode.Single);
        new GameObject("Roost Physics Comparison").AddComponent<PhysicsComparison>();
        Directory.CreateDirectory("Assets/Scenes");
        EditorSceneManager.SaveScene(UnityEngine.SceneManagement.SceneManager.GetActiveScene(),
                                     "Assets/Scenes/PhysicsComparison.unity");
        EditorBuildSettings.scenes =
            new[] { new EditorBuildSettingsScene("Assets/Scenes/PhysicsComparison.unity", true) };
        AssetDatabase.SaveAssets();
        CreateCrowdScene();
        CreateRoomScene();
        EditorSceneManager.OpenScene("Assets/Scenes/PhysicsComparison.unity");
        Debug.Log("ROOST_VIEWER_SCENE_READY");
    }
    // 必须在 Play Mode 运行独立 PhysicsScene；批处理没有 -quit，由验收回调退出。
    public static void VerifyConnection()
    {
        EditorSceneManager.OpenScene("Assets/Scenes/PhysicsComparison.unity");
        SessionState.SetBool(Verification, true);
        SessionState.SetString(Verification + "Deadline", DateTime.UtcNow.AddSeconds(60).ToString("O"));
        EditorApplication.isPlaying = true;
    }
    public static void CreateCrowdScene()
    {
        EditorSceneManager.NewScene(NewSceneSetup.EmptyScene, NewSceneMode.Single);
        new GameObject("Roost Crowd Viewer").AddComponent<CrowdViewer>();
        Directory.CreateDirectory("Assets/Scenes");
        EditorSceneManager.SaveScene(UnityEngine.SceneManagement.SceneManager.GetActiveScene(), "Assets/Scenes/Crowd.unity");
        AssetDatabase.SaveAssets();
    }
    public static void VerifyCrowd()
    {
        CreateCrowdScene();
        SessionState.SetBool(Verification + "Crowd", true);
        SessionState.SetBool(Verification, true);
        SessionState.SetString(Verification + "Deadline", DateTime.UtcNow.AddSeconds(60).ToString("O"));
        EditorApplication.isPlaying = true;
    }
    public static void CreateRoomScene()
    {
        EditorSceneManager.NewScene(NewSceneSetup.EmptyScene, NewSceneMode.Single);
        var viewer=new GameObject("Roost Room Controller").AddComponent<CrowdViewer>();
        viewer.ControlRoom=true;viewer.Port=19093;viewer.PlayerSlot=13;
        Directory.CreateDirectory("Assets/Scenes");
        EditorSceneManager.SaveScene(UnityEngine.SceneManagement.SceneManager.GetActiveScene(), "Assets/Scenes/Room.unity");
        AssetDatabase.SaveAssets();
    }
    public static void OpenRoomInteractive()
    {
        if (!File.Exists("Assets/Scenes/Room.unity")) CreateRoomScene();
        EditorSceneManager.OpenScene("Assets/Scenes/Room.unity");
        EditorApplication.isPlaying=true;
    }
    public static void VerifyRoom()
    {
        CreateRoomScene();
        UnityEngine.Object.FindFirstObjectByType<CrowdViewer>().VerifyControls=true;
        SessionState.SetBool(Verification+"Capture",false);
        SessionState.SetBool(Verification+"Room",true);
        SessionState.SetBool(Verification,true);
        SessionState.SetString(Verification+"Deadline",DateTime.UtcNow.AddSeconds(60).ToString("O"));
        if(File.Exists("Artifacts/room.png")) File.Delete("Artifacts/room.png");
        EditorApplication.isPlaying=true;
    }
    static void CheckVerification()
    {
        if (!SessionState.GetBool(Verification, false))
            return;
        var deadline = DateTime.Parse(SessionState.GetString(Verification + "Deadline", ""), null,
                                      System.Globalization.DateTimeStyles.RoundtripKind);
        if (DateTime.UtcNow > deadline)
        {
            var room = UnityEngine.Object.FindFirstObjectByType<CrowdViewer>();
            Finish(false, "Unity verification timeout " + (room == null ? "no viewer" : $"{room.Summary} travel={room.HorizontalTravel} rise={room.VerticalRise} jumps={room.JumpRequests}"));
            return;
        }
        if (!EditorApplication.isPlaying || EditorApplication.isCompiling)
            return;
        if (SessionState.GetBool(Verification+"Room",false))
        {
            var room=UnityEngine.Object.FindFirstObjectByType<CrowdViewer>();
            if(room==null)return;
            if(room.Status!="Connected"&&room.Status!="Connecting"){Finish(false,room.Status);return;}
            if(room.Frames>=200 && room.HorizontalTravel>2 && room.VerticalRise>.8f && room.JumpRequests>=2)
            {
                if(!SessionState.GetBool(Verification+"Capture",false))
                {
                    Directory.CreateDirectory("Artifacts");
                    Capture("Artifacts/room.png");
                    SessionState.SetBool(Verification+"Capture",true);
                }
                else if(File.Exists("Artifacts/room.png")) Finish(true,$"{room.Summary} travel={room.HorizontalTravel:F2} rise={room.VerticalRise:F2} jumps={room.JumpRequests}");
            }
            return;
        }
        if (SessionState.GetBool(Verification + "Crowd", false))
        {
            var crowd = UnityEngine.Object.FindFirstObjectByType<CrowdViewer>();
            if (crowd == null) return;
            if (crowd.Status != "Connected" && crowd.Status != "Connecting") { Finish(false, crowd.Status); return; }
            if (crowd.Frames >= 200) { Capture(); Finish(true, crowd.Summary); }
            return;
        }
        var viewer = UnityEngine.Object.FindFirstObjectByType<PhysicsComparison>();
        if (viewer == null)
            return;
        if (viewer.Status != "Connected" && viewer.Status != "Connecting")
        {
            Finish(false, viewer.Status);
            return;
        }
        if (viewer.ComparedFrames >= 240 && viewer.ComparedTick >= 599)
        {
            viewer.FlushMetrics();
            Capture();
            Finish(true, viewer.VerificationSummary);
        }
    }
    static void Capture(string path = "Artifacts/comparison.png")
    {
        if (SystemInfo.graphicsDeviceType == UnityEngine.Rendering.GraphicsDeviceType.Null)
            return;
        Directory.CreateDirectory("Artifacts");
        var image = new Texture2D(1280, 720, TextureFormat.RGB24, false);
        foreach (var camera in UnityEngine.Object.FindObjectsByType<Camera>(FindObjectsSortMode.None))
        {
            int offset = Mathf.RoundToInt(camera.rect.x * 1280);
            int width = Mathf.RoundToInt(camera.rect.width * 1280);
            var original = camera.rect;
            var target = new RenderTexture(width, 720, 24);
            camera.targetTexture = target;
            camera.rect = new Rect(0, 0, 1, 1);
            camera.Render();
            RenderTexture.active = target;
            image.ReadPixels(new Rect(0, 0, width, 720), offset, 0);
            camera.targetTexture = null;
            camera.rect = original;
            RenderTexture.active = null;
            target.Release();
            UnityEngine.Object.DestroyImmediate(target);
        }
        image.Apply();
        File.WriteAllBytes(path, image.EncodeToPNG());
        UnityEngine.Object.DestroyImmediate(image);
    }
    static void Finish(bool ok, string result)
    {
        SessionState.SetBool(Verification, false);
        SessionState.SetBool(Verification+"Room",false);
        SessionState.SetBool(Verification+"Capture",false);
        SessionState.SetBool(Verification + "Crowd", false);
        Debug.Log((ok ? "ROOST_VIEWER_TCP_PHYSICS_PASS " : "ROOST_VIEWER_TCP_PHYSICS_FAIL ") + result);
        EditorApplication.Exit(ok ? 0 : 1);
    }
}
