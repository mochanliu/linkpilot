#!/bin/bash
# 编译 SR-WG VPP 插件
# 用法: build-plugin.sh [plugin_source_dir]

set -e

PLUGIN_DIR=${1:-/workspace/vpp-plugin}
BUILD_DIR=${PLUGIN_DIR}/build

echo "=== Building SR-WG plugin from ${PLUGIN_DIR} ==="

# 检测 VPP CMake 配置文件
VPP_CMAKE_PATHS=(
    "/usr/lib/x86_64-linux-gnu/cmake/vpp/VPPConfig.cmake"
    "/usr/lib64/cmake/vpp/VPPConfig.cmake"
    "/usr/share/cmake/vpp/VPPConfig.cmake"
)

VPP_FOUND=0
for path in "${VPP_CMAKE_PATHS[@]}"; do
    if [ -f "$path" ]; then
        echo "Found VPP CMake config: $path"
        VPP_FOUND=1
        break
    fi
done

# 也检查 cmake 模块搜索路径
if [ ${VPP_FOUND} -eq 0 ]; then
    if find /usr -name "VPPConfig.cmake" -print -quit 2>/dev/null | grep -q .; then
        echo "Found VPP CMake config via find"
        VPP_FOUND=1
    fi
fi

mkdir -p ${BUILD_DIR}
cd ${BUILD_DIR}

if [ ${VPP_FOUND} -eq 1 ]; then
    echo "Using VPP CMake build system..."
    cmake ${PLUGIN_DIR} \
        -DCMAKE_BUILD_TYPE=Release \
        -G Ninja
    ninja
    echo "=== Build complete ==="
    echo "Plugin artifacts in ${BUILD_DIR}/lib/vpp_plugins"
    # add_vpp_plugin 把产物放在 ${CMAKE_LIBRARY_OUTPUT_DIRECTORY}/vpp_plugins，
    # CMakeLists.txt 将 CMAKE_LIBRARY_OUTPUT_DIRECTORY 设为 ${CMAKE_BINARY_DIR}/lib
    find ${BUILD_DIR} -name '*.so' -print
else
    echo "WARNING: VPP CMake config not found!"
    echo "Searched paths:"
    for path in "${VPP_CMAKE_PATHS[@]}"; do
        echo "  - $path"
    done
    echo ""
    echo "Falling back to standalone library build (for unit tests)..."
    cd ${PLUGIN_DIR}
    make clean && make lib
    echo "=== Library build complete (plugin build requires VPP dev packages) ==="
fi
