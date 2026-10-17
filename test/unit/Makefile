# Makefile - SR Header 核心库单元测试
#
# 直接编译被测源码（sr_hmac.c / sr_siphash.c / sr_path.c）与测试源码。
# 签名使用内置 SipHash-2-4，无外部依赖；sr_header.h 为纯头文件
# （inline 函数），无需单独编译。
#
# 目标：
#   make          构建测试可执行文件
#   make test     构建并运行测试
#   make clean    清理构建产物

CC       ?= gcc
PLUGIN_DIR := ../../vpp-plugin
BUILD_DIR  := build
TARGET     := test_srwg

CFLAGS   := -Wall -Wextra -std=c11 -g -O0 -I$(PLUGIN_DIR)
LDFLAGS  :=
LDLIBS   :=

# 测试源码 + 被测库源码
TEST_SRCS := test_main.c test_sr_header.c test_sr_hmac.c test_sr_siphash.c test_sr_path.c
LIB_SRCS  := $(PLUGIN_DIR)/sr_hmac.c $(PLUGIN_DIR)/sr_siphash.c $(PLUGIN_DIR)/sr_path.c
SRCS      := $(TEST_SRCS) $(LIB_SRCS)

# 所有 .o 统一放入 build/（用 notdir 避免路径冲突）
OBJS := $(patsubst %.c,$(BUILD_DIR)/%.o,$(notdir $(SRCS)))

# 头文件依赖（简化：任意头文件变动即重编）
HDRS := test_framework.h \
        $(PLUGIN_DIR)/sr_header.h \
        $(PLUGIN_DIR)/sr_hmac.h \
        $(PLUGIN_DIR)/sr_siphash.h \
        $(PLUGIN_DIR)/sr_path.h

.PHONY: all test clean
all: $(TARGET)

$(TARGET): $(OBJS)
	$(CC) $(LDFLAGS) -o $@ $^ $(LDLIBS)

# vpath 让 make 能在当前目录与 PLUGIN_DIR 中查找同名 .c
vpath %.c .:$(PLUGIN_DIR)

$(BUILD_DIR)/%.o: %.c $(HDRS) | $(BUILD_DIR)
	$(CC) $(CFLAGS) -c -o $@ $<

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

test: $(TARGET)
	./$(TARGET)

clean:
	rm -rf $(BUILD_DIR) $(TARGET)
