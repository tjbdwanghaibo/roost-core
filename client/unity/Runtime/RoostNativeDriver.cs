using System;
using Roost.Native;
using UnityEngine;
namespace Roost.Unity
{
// 连接与回调归 Unity 主线程。取票/DNS 由业务异步准备，恢复前台不重放旧请求。
public sealed class RoostNativeDriver : MonoBehaviour
{
    [SerializeField]
    private int callbackBudget = 64;
    public NativeGameClient Client { get; private set; }
    public NativeReconnect Reconnect { get; private set; }
    public event Action ResumeRequested;
    private bool paused;
    private bool disabled;
    public void Attach(NativeGameClient client, NativeReconnect reconnect = null)
    {
        if (client == null)
            throw new ArgumentNullException(nameof(client));
        if (!isActiveAndEnabled || paused)
            throw new InvalidOperationException("Attach requires an active foreground driver");
        if (ReferenceEquals(Client, client))
            return;
        Detach();
        Client = client;
        Reconnect = reconnect;
    }
    public void Detach()
    {
        Reconnect?.Dispose();
        Reconnect = null;
        Client?.Dispose();
        Client = null;
    }
    private void Update()
    {
        if (paused)
            return;
        try
        {
            Client?.Pump(Math.Max(1, callbackBudget));
            Reconnect?.Tick();
        }
        catch (Exception error)
        {
            Debug.LogException(error);
        }
    }
    private void OnApplicationPause(bool value)
    {
        if (paused == value)
            return;
        paused = value;
        if (value)
        {
            Reconnect?.Stop();
            Client?.Disconnect();
        }
        else if (isActiveAndEnabled)
            ResumeRequested?.Invoke();
    }
    private void OnDisable()
    {
        disabled = true;
        Reconnect?.Stop();
        Client?.Disconnect();
    }
    private void OnEnable()
    {
        if (disabled && !paused)
            ResumeRequested?.Invoke();
        disabled = false;
    }
    private void OnDestroy()
    {
        Detach();
    }
}
}
