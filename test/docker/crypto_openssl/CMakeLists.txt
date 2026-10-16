##############################################################################
# crypto_openssl - 最小 OpenSSL 同步 crypto 引擎插件
#
# 为 packagecloud VPP 26.06-release 补齐缺失的 ChaCha20-Poly1305 同步(SIMPLE)
# handler，WireGuard 握手/数据面依赖它。
#
# 构建方式（容器内，已安装 vpp-dev）：
#   mkdir -p build && cd build
#   cmake .. -DCMAKE_BUILD_TYPE=Release -G Ninja
#   ninja
#   产物: build/lib/vpp_plugins/crypto_openssl_plugin.so
##############################################################################
cmake_minimum_required(VERSION 3.16)
project(crypto_openssl_plugin C)

set(CMAKE_C_STANDARD 11)
set(CMAKE_C_STANDARD_REQUIRED ON)

find_package(VPP REQUIRED)
find_package(OpenSSL REQUIRED COMPONENTS Crypto)

# add_vpp_plugin 把产物放到 ${CMAKE_LIBRARY_OUTPUT_DIRECTORY}/vpp_plugins
set(CMAKE_LIBRARY_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/lib)
set(CMAKE_ARCHIVE_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/lib)
set(CMAKE_RUNTIME_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/bin)

add_vpp_plugin(crypto_openssl
  SOURCES
    crypto_openssl.c
  LINK_LIBRARIES
    OpenSSL::Crypto
)
