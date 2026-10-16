# SR-WG VPP 插件开发环境镜像
# 基于 Ubuntu 22.04，使用 VPP 官方包仓库（packagecloud.io/fdio）安装，
# 比源码编译快得多且稳定。
#
# 备选方案：如果 packagecloud 的 VPP 仓库不可用或版本不对，改用源码编译方式：
#   RUN git clone https://gerrit.fd.io/r/vpp /tmp/vpp \
#       && cd /tmp/vpp && git checkout stable/2406 \
#       && make install-dep && make build-release \
#       && make install
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

# 基础工具
RUN apt-get update && apt-get install -y \
    curl wget gnupg apt-transport-https ca-certificates \
    build-essential gcc clang cmake ninja-build \
    git python3 python3-pip python3-venv \
    iproute2 iputils-ping tcpdump net-tools \
    libssl-dev libnuma-dev \
    sudo vim \
    && rm -rf /var/lib/apt/lists/*

# 安装 VPP 官方仓库（24.06 stable 或最新 release）
RUN curl -s https://packagecloud.io/install/repositories/fdio/release/script.deb.sh | bash

# 安装 VPP 及开发包
# 注：fdio 仓库中无 vpp-lib 包（库文件包含在 vpp 中），Python API 包名为 python3-vpp-api
#     python3-ply 是 vppapigen 解析 .api 文件的运行时依赖，vpp-dev 未声明该依赖，
#     缺失时编译会在 "Generating API header" 阶段报 ModuleNotFoundError: No module named 'ply'
RUN apt-get update && apt-get install -y \
    vpp \
    vpp-dev \
    vpp-plugin-core \
    vpp-plugin-dpdk \
    vpp-plugin-devtools \
    vpp-drivers \
    libvppinfra \
    libvppinfra-dev \
    python3-vpp-api \
    python3-ply \
    && rm -rf /var/lib/apt/lists/*

# Go 环境（用于控制器开发和 binapi 生成）
# 依次尝试官方源和国内镜像，避免单一源网络不可达导致构建失败
RUN GO_TGZ=go1.22.5.linux-amd64.tar.gz; \
    (wget -q -O ${GO_TGZ} https://go.dev/dl/${GO_TGZ} \
     || wget -q -O ${GO_TGZ} https://golang.google.cn/dl/${GO_TGZ} \
     || wget -q -O ${GO_TGZ} https://mirrors.aliyun.com/golang/${GO_TGZ}) \
    && tar -C /usr/local -xzf ${GO_TGZ} \
    && rm ${GO_TGZ}
ENV PATH="/usr/local/go/bin:${PATH}"
ENV GOPATH="/root/go"

# 工作目录
WORKDIR /workspace

# VPP 启动配置（使用 tap 接口，Docker 环境无巨页）
COPY vpp-startup.conf /etc/vpp/startup.conf

# 插件编译脚本
COPY build-plugin.sh /usr/local/bin/build-plugin.sh
RUN chmod +x /usr/local/bin/build-plugin.sh

# ---------------------------------------------------------------------------
# 构建并安装 crypto_openssl 同步 crypto 引擎插件
#
# packagecloud 的 VPP 26.06-release 只打包了 crypto_sw_scheduler（纯 *异步*
# 引擎），整个运行时没有任何“同步(SIMPLE)”crypto handler。而 WireGuard 插件
# 的 noise 握手与数据面都通过 vnet_crypto_process_ops() 的 *同步* 路径调用
# ChaCha20-Poly1305；缺失 SIMPLE handler 时该调用是静默 no-op，握手报文的
# encrypted_static / encrypted_timestamp 全为零，对端解密失败并报 "Peer error"，
# 握手永远无法建立。
#
# 下面用 OpenSSL EVP 补齐一个最小的同步 ChaCha20-Poly1305 引擎（注册为名为
# "openssl" 的 crypto engine），装进镜像的 vpp_plugins 目录，VPP 启动即自动加载。
# 仅需 vpp-dev 头文件 + libssl-dev + cmake/ninja（上面已安装），构建无需外网。
# 放在 Dockerfile 末尾，避免失效前面已缓存的 VPP/Go 安装层。
# ---------------------------------------------------------------------------
COPY crypto_openssl /tmp/crypto_openssl
RUN mkdir -p /tmp/crypto_openssl/build && cd /tmp/crypto_openssl/build \
    && cmake .. -DCMAKE_BUILD_TYPE=Release -G Ninja \
    && ninja \
    && install -m 0644 lib/vpp_plugins/crypto_openssl_plugin.so \
         /usr/lib/x86_64-linux-gnu/vpp_plugins/crypto_openssl_plugin.so \
    && rm -rf /tmp/crypto_openssl

EXPOSE 51820/udp 51821/udp

CMD ["/bin/bash"]
