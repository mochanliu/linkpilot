/*
 * test_framework.h - 极简自定义单元测试框架（无外部依赖）
 *
 * 计数器使用 extern 声明，统一定义在 test_main.c 中，
 * 以便跨多个翻译单元（test_sr_header.c / test_sr_hmac.c /
 * test_sr_path.c）共享同一份统计结果。
 *
 * 约定：每个测试函数签名为 void func(void)，
 *       TEST_ASSERT 失败时会打印信息并 return，终止当前用例。
 */
#ifndef TEST_FRAMEWORK_H
#define TEST_FRAMEWORK_H

#include <stdio.h>
#include <string.h>
#include <stdint.h>

/* 全局测试统计（定义在 test_main.c） */
extern int tests_run;
extern int tests_passed;
extern int tests_failed;

/* 断言：失败即打印并 return，终止当前测试函数 */
#define TEST_ASSERT(cond, msg) do {                                   \
    tests_run++;                                                      \
    if (!(cond)) {                                                    \
        printf("  FAIL: %s (line %d): %s\n", __func__, __LINE__, msg);\
        tests_failed++;                                               \
        return;                                                       \
    } else {                                                          \
        tests_passed++;                                               \
    }                                                                 \
} while (0)

#define TEST_ASSERT_EQ(a, b, msg)  TEST_ASSERT((a) == (b), msg)
#define TEST_ASSERT_NEQ(a, b, msg) TEST_ASSERT((a) != (b), msg)
#define TEST_ASSERT_NULL(a, msg)   TEST_ASSERT((a) == NULL, msg)
#define TEST_ASSERT_NOT_NULL(a, msg) TEST_ASSERT((a) != NULL, msg)
#define TEST_ASSERT_MEM_EQ(a, b, len, msg) \
    TEST_ASSERT(memcmp((a), (b), (len)) == 0, msg)

/* 运行单个测试用例 */
#define RUN_TEST(func) do {                                           \
    printf("  Running %s...\n", #func);                               \
    func();                                                           \
} while (0)

#define TEST_SUITE_BEGIN(name) printf("\n=== %s ===\n", (name))
#define TEST_SUITE_END()       printf("  Suite complete.\n")

#endif /* TEST_FRAMEWORK_H */
