using UnrealBuildTool;
using System.IO;
// Place the public headers and per-platform static library in this external module.
public class RoostClient : ModuleRules
{
    public RoostClient(ReadOnlyTargetRules Target) : base(Target)
    {
        Type = ModuleType.External;
        PublicIncludePaths.Add(Path.Combine(ModuleDirectory, "include"));
        string platform = Target.Platform.ToString();
        string library = Target.Platform == UnrealTargetPlatform.Win64 ? "roost_client.lib" : "libroost_client.a";
        PublicAdditionalLibraries.Add(Path.Combine(ModuleDirectory, "lib", platform, library));
        if (Target.Platform == UnrealTargetPlatform.Win64) PublicSystemLibraries.Add("ws2_32.lib");
        // Use client.h from UE when C++ exceptions are disabled. The C ABI never exposes STL.
    }
}
