# world-basemap 底图生成器

`controller/cmd/sr-ctrl/assets/world-110m.js` 与 `world-50m.js` 的来源。
这两个文件是**生成物且已入库**，所以 `go build` 不需要 node，也不需要联网 —— 这是
air-gap 交付的前提。只有要改分辨率、纬度裁剪、视口大小或精度时才需要重跑本脚本。

## 跑一次

```bash
node gen.mjs [--vendor DIR] [--out DIR]
```

`--vendor` 默认 `./vendor`，`--out` 默认 `../../cmd/sr-ctrl/assets`。
本机没有 node，用 Qoder 自带的：

```bash
ELECTRON_RUN_AS_NODE=1 "/Applications/Qoder CN.app/Contents/MacOS/Qoder CN" gen.mjs
```

## vendor/ 里放什么

`vendor/` **不入库**（第三方输入物，且生成物已经进了仓库）。六个文件，全部 ISC / 公有领域：

| 文件 | 来自 | 许可证 |
| --- | --- | --- |
| `d3-array.min.js` | d3-array v3.2.4 `dist/`（UMD build） | ISC |
| `d3-geo.min.js` | d3-geo v3.1.1 `dist/`（UMD build） | ISC |
| `topojson-client.min.js` | topojson-client v3.1.0 `dist/`（UMD build） | ISC |
| `countries-110m.json`、`land-110m.json` | world-atlas 的 Natural Earth **1:110m** topojson | 数据公有领域，封装 ISC |
| `land-50m.json` | world-atlas 的 Natural Earth **1:50m** topojson | 数据公有领域，封装 ISC |

版本以**字节**钉住，见同目录的 `SHA256SUMS`：

```bash
cd vendor && shasum -a 256 -c ../SHA256SUMS   # 六个文件都要 OK
```

world-atlas 的 JSON 里没有自带版本号，所以按哈希而不是按版本记 —— 换一份数据文件，哈希就对不上，
正好能看到自己换了什么。三个 JS 是 UMD build，`gen.mjs` 用 `vm` 求值后取 `globalThis.d3` /
`globalThis.topojson`；地图数据只被 `JSON.parse`，不会被执行。

## 产出与体积

| 文件 | 全局名 | 内容 | 未压缩 | gzip |
| --- | --- | --- | --- | --- |
| `world-110m.js` | `window.SR_WORLD` | land + borders（已 merge，一条共享边界只描一次）+ 20° graticule + 投影参数 | 约 132 KB | 约 53 KB |
| `world-50m.js` | `window.SR_WORLD_50M` | 只有 land | 约 680 KB | 约 213 KB |

视口 2000×1040，四周留 4px，纬度裁剪 `>= -60°`（南极冰架以下整片是空白海）。坐标精度 0.1px。
50m 那层**不带投影参数**：它和 110m 用同一个 fitExtent 结果生成，所以放大后两层严格套在一起；
如果各算各的 fitExtent，两个 bbox 差几毫度就会让海岸线错开几像素。

## 脚本里那个检查

设备点位用的是控制台自己的 12 行 Natural Earth I 正算公式（运行时不用任何库）。
`gen.mjs` 末尾拿 9 个城市比对 d3 与手算结果，超过 **0.01px 就直接退出** ——
两者一旦不一致，每个点都会偏出它自己的国家，而症状看起来像"坐标填错了"，不像 bug。
这就是底图必须是生成物、不能手描的原因。

## 换了东西之后

改完重新生成，**两件事要跟着做**：
1. `go test ./cmd/sr-ctrl`（`console_assets_test.go` 里钉着 assets 的可见性与体积）；
2. 打开控制台拓扑页在真实浏览器里看一遍：整幅、放大到 12×、以及只有一台设备定位时的样子。
