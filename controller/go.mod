module sdwan/controller

// 零第三方依赖：enroll/注册表/watch 全部走标准库。
//
// develop 上的控制器模块（srou）把 go 指令抬到了 1.24.0，唯一原因是 govpp
// v0.13.0 声明了 `go 1.24.0`。本模块不含 govpp —— VPP 侧的编程入口走 vppctl，
// 因此这里可以停留在容器实际装的 Go 1.22.5。
go 1.22
