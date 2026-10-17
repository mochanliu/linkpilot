##############################################################################
# SR-WG VPP Plugin - CMakeLists.txt
#
# 构建方式：
#   mkdir -p build && cd build
#   cmake .. -DCMAKE_BUILD_TYPE=Release -G Ninja
#   ninja
##############################################################################
cmake_minimum_required(VERSION 3.16)
project(sr_wg_plugin C)

set(CMAKE_C_STANDARD 11)
set(CMAKE_C_STANDARD_REQUIRED ON)

# 查找 VPP 构建基础设施
find_package(VPP REQUIRED)

# 签名算法为内置 SipHash-2-4，不依赖 OpenSSL

##############################################################################
# 输出目录
#
# VPP 的 add_vpp_plugin / add_vpp_test_library 宏把产物放在
#   ${CMAKE_LIBRARY_OUTPUT_DIRECTORY}/vpp_plugins
#   ${CMAKE_LIBRARY_OUTPUT_DIRECTORY}/vat2_plugins
# 该变量必须由本项目设置，否则为空串，.so 会被写到文件系统根目录
# （/vpp_plugins），既不在构建树内也无法被部署脚本找到。
##############################################################################
set(CMAKE_LIBRARY_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/lib)
set(CMAKE_ARCHIVE_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/lib)
set(CMAKE_RUNTIME_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/bin)

##############################################################################
# 插件源文件
##############################################################################
set(SR_WG_PLUGIN_SOURCES
  sr_wg.c
  sr_key.c
  sr_wg4_transit.c
  sr_wg4_input.c
  sr_wg4_encap.c
  sr_wg_api.c
  sr_wg_cli.c
  sr_hmac.c
  sr_siphash.c
  sr_path.c
  sr_obs.c
)

##############################################################################
# API 文件
##############################################################################
set(SR_WG_API_FILES
  sr_wg.api
)

##############################################################################
# 使用 VPP 的 add_vpp_plugin 宏构建插件
##############################################################################
add_vpp_plugin(sr_wg
  SOURCES ${SR_WG_PLUGIN_SOURCES}
  API_FILES ${SR_WG_API_FILES}
  INSTALL_HEADERS
    sr_header.h
    sr_hmac.h
    sr_siphash.h
    sr_path.h
    sr_obs.h
    sr_counters.h
    sr_wg.h
)
