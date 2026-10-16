CC      = gcc
CFLAGS  = -Wall -Wextra -std=c11 -O2 -I.
LDFLAGS = -lcrypto

SRCS = sr_hmac.c sr_path.c
OBJS = $(SRCS:.c=.o)
DEPS = sr_header.h sr_hmac.h sr_path.h sr_counters.h
LIB  = libsrwg.a

BUILD_DIR = build

# ==================== 目标 ====================

.PHONY: all lib plugin clean clean-plugin

all: lib

# 独立编译静态库（单元测试用）
lib: $(LIB)

$(LIB): $(OBJS)
	ar rcs $@ $^

%.o: %.c $(DEPS)
	$(CC) $(CFLAGS) -c -o $@ $<

# VPP 插件编译（在 Docker 或安装了 VPP 开发包的环境中使用）
plugin:
	mkdir -p $(BUILD_DIR) && \
	cd $(BUILD_DIR) && \
	cmake .. -DCMAKE_BUILD_TYPE=Release -G Ninja && \
	ninja

clean:
	rm -f $(OBJS) $(LIB)

clean-plugin:
	rm -rf $(BUILD_DIR)
