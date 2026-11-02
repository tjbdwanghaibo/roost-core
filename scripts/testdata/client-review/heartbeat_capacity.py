"""Run the C# heartbeat capacity review probe against unchanged client sources.
Usage: python3 heartbeat_capacity.py REVIEWED_REPOSITORY DOTNET_ROOT
Requires an installed .NET 10 SDK. Baseline exits 1; a fix should exit 0.
The direct compiler entry avoids SDK first-run/NuGet writes outside the workspace.
"""
import json
import pathlib
import subprocess
import sys
import tempfile

root = pathlib.Path(sys.argv[1]).resolve()
dotnet = pathlib.Path(sys.argv[2]).resolve()
def version_key(path):
    return tuple(int(part) for part in path.name.split("-")[0].split("."))
sdk = max((p for p in (dotnet / "sdk").iterdir() if p.name.startswith("10.")), key=version_key)
pack = max((p for p in (dotnet / "packs/Microsoft.NETCore.App.Ref").iterdir() if p.name.startswith("10.")), key=version_key)
refs = sorted((pack / "ref/net10.0").glob("*.dll"))
assert refs, "Missing .NET 10 reference assemblies"
with tempfile.TemporaryDirectory(prefix="review-heartbeat-") as temporary:
    work = pathlib.Path(temporary)
    (work / "Program.cs").write_text(pathlib.Path(__file__).with_name("heartbeat_capacity.cs.txt").read_text())
    output = work / "probe.dll"
    command = [str(dotnet / "dotnet"), str(sdk / "Roslyn/bincore/csc.dll"),
               "-nologo", "-target:exe", "-nullable:enable", "-out:" + str(output)]
    command += ["-r:" + str(reference) for reference in refs]
    command += [str(source) for source in sorted((root / "client/dotnet/Roost.Client").glob("*.cs"))]
    command += [str(work / "Program.cs")]
    subprocess.run(command, check=True)
    (work / "probe.runtimeconfig.json").write_text(json.dumps({"runtimeOptions": {
        "tfm": "net10.0", "framework": {"name": "Microsoft.NETCore.App", "version": "10.0.0"}}}))
    result = subprocess.run([str(dotnet / "dotnet"), str(output)], timeout=30)
    sys.exit(result.returncode)
