using UnityEditor;
namespace Roost.Unity.Editor
{
// 插件导入规则跟包走，避免每个游戏工程手工误选 CPU 或把移动库载入 Editor。
public sealed class NativePluginImporter : AssetPostprocessor
{
    private void OnPreprocessAsset()
    {
        if (!(assetImporter is PluginImporter plugin) || !assetPath.Contains("com.roost.client/Runtime/Plugins/"))
            return;
        plugin.SetCompatibleWithAnyPlatform(false);
        plugin.SetCompatibleWithEditor(false);
        if (assetPath.Contains("/macOS/"))
        {
            plugin.SetCompatibleWithEditor(true);
            plugin.SetEditorData("OS", "OSX");
            plugin.SetEditorData("CPU", "ARM64");
            plugin.SetCompatibleWithPlatform(BuildTarget.StandaloneOSX, true);
            plugin.SetPlatformData(BuildTarget.StandaloneOSX, "CPU", "ARM64");
        }
        else if (assetPath.Contains("/Windows/"))
        {
            plugin.SetCompatibleWithEditor(true);
            plugin.SetEditorData("OS", "Windows");
            plugin.SetEditorData("CPU", "x86_64");
            plugin.SetCompatibleWithPlatform(BuildTarget.StandaloneWindows64, true);
        }
        else if (assetPath.Contains("/Linux/"))
        {
            plugin.SetCompatibleWithEditor(true);
            plugin.SetEditorData("OS", "Linux");
            plugin.SetEditorData("CPU", "x86_64");
            plugin.SetCompatibleWithPlatform(BuildTarget.StandaloneLinux64, true);
        }
        else if (assetPath.Contains("/Android/"))
        {
            plugin.SetCompatibleWithPlatform(BuildTarget.Android, true);
            plugin.SetPlatformData(BuildTarget.Android, "CPU", "ARM64");
        }
        else if (assetPath.Contains("/iOS/"))
            plugin.SetCompatibleWithPlatform(BuildTarget.iOS, true);
    }
}
}
