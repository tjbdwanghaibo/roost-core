#!/usr/bin/env bash
# 固定官方源码；补丁仅处理现代编译器与 Apple Silicon 构建，不改变求解器。
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
deps=${ROOST_PHYSICS_DEPS:-"$root/artifacts/physics-deps"}
src="$deps/PhysX-4.1"
revision=a2c0428acab643e60618c681b501e86f7fd558cc
if [[ ! -d "$src/.git" ]]; then
 mkdir -p "$deps"
 git -c filter.lfs.process= -c filter.lfs.smudge= -c filter.lfs.required=false clone --depth 1 --branch 4.1 https://github.com/NVIDIAGameWorks/PhysX.git "$src"
fi
[[ $(git -C "$src" rev-parse HEAD) == "$revision" ]] || { echo 'Unexpected PhysX source revision' >&2; exit 1; }
python3 - "$src" <<'PY'
import pathlib,sys,platform,subprocess
root=pathlib.Path(sys.argv[1])
for name in ('mac','linux'):
 p=root/'physx/source/compiler/cmake'/name/'CMakeLists.txt'
 text=p.read_text().replace('-Werror ', '').replace('-Weverything ', '')
 if name=='mac' and platform.machine()=='arm64':
  text=text.replace('-arch x86_64','-arch arm64').replace('"x86_64"','"arm64"').replace('-msse2 ','').replace('"10.9"','"11.0"')
 if p.read_text()!=text:p.write_text(text)
# 新版 clang 在未选中的模板分支也校验 NEON lane；改用同函数已有的常量 lane 分支。
p=root/'physx/source/foundation/include/unix/neon/PsUnixNeonInlineAoS.h'
s=subprocess.check_output(['git','-C',str(root),'show','HEAD:physx/source/foundation/include/unix/neon/PsUnixNeonInlineAoS.h'],text=True)
s=s.replace('Vec4V V4SplatElement(Vec4V a)\n{\n#if PX_UWP','Vec4V V4SplatElement(Vec4V a)\n{\n#if PX_UWP || PX_A64')
if p.read_text()!=s:p.write_text(s)
PY
cmake -S "$root/infra/physics/physx/native" -B "$deps/build" -G Ninja \
 -DCMAKE_BUILD_TYPE=release -DPHYSX_SOURCE="$src" -DROOST_LIB_DIR="$root/infra/physics/physx/lib"
cmake --build "$deps/build" --target roost_physx -j "${ROOST_BUILD_JOBS:-4}"
cp "$src/LICENSE.md" "$root/infra/physics/physx/lib/PhysX-LICENSE.md"
