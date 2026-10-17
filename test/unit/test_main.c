/*
 * test_main.c - SR Header 核心库单元测试统一入口
 *
 * 定义全局测试计数器，依次运行五个测试套件，输出 PASS/FAIL 统计。
 * 退出码：全部通过返回 0，存在失败返回 1（便于 CI 集成）。
 */
#include "test_framework.h"

/* 全局测试计数器（test_framework.h 中以 extern 声明） */
int tests_run    = 0;
int tests_passed = 0;
int tests_failed = 0;

/* 各测试套件入口 */
extern void test_suite_sr_header(void);
extern void test_suite_sr_siphash(void);
extern void test_suite_sr_hmac(void);
extern void test_suite_sr_path(void);
extern void test_suite_sr_obs(void);

int main(void)
{
    printf("====================================\n");
    printf("  SR Header 核心库单元测试\n");
    printf("====================================\n");

    test_suite_sr_header();
    test_suite_sr_siphash();
    test_suite_sr_hmac();
    test_suite_sr_path();
    test_suite_sr_obs();

    printf("\n====================================\n");
    printf("  测试汇总\n");
    printf("------------------------------------\n");
    printf("  总断言数 : %d\n", tests_run);
    printf("  通过     : %d\n", tests_passed);
    printf("  失败     : %d\n", tests_failed);
    printf("====================================\n");

    if (tests_failed == 0) {
        printf("  结果: ALL PASS ✅\n");
        return 0;
    }
    printf("  结果: FAILED ❌\n");
    return 1;
}
