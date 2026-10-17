# NOTICE — 第三方代码与字体许可

本文件列出**直接依赖**及其许可，以及内嵌资产的许可。新增任何依赖必须同时更新本文件（AGENTS：新增依赖要先问）。

| 组件 | 版本 | 用途 | 许可 | 位置 |
|---|---|---|---|---|
| `modernc.org/sqlite` | v1.46.1 | 纯 Go SQLite 驱动（防重放账本、题池、图片 BLOB） | BSD-3-Clause（SQLite 本体 public domain） | go.mod |
| `golang.org/x/image` | v0.36.0 | `font/opentype` + `font/gofont/goregular` + `draw`（字形光栅与旋转合成） | BSD-3-Clause © 2009 The Go Authors | go.mod |
| Go 字体（"Go Regular"） | 随 x/image 内嵌 | 视觉题的字形来源（`goregular.TTF`，148,672 字节） | BSD-3-Clause © 2016 Bigelow & Holmes Inc. | `golang.org/x/image/font/gofont/ttfs/README` |
| `golang.org/x/text` | v0.34.0（**传递依赖**，我们不直接 import） | x/image/font/sfnt 的编码表 | BSD-3-Clause | go.mod indirect |

## 版本选择的硬约束

- `golang.org/x/image` 锁在 **v0.36.0**：它的 `go` 指令是 `go 1.24.0`，与本机 go1.24.5 兼容。
  **v0.37.0 起要求 go ≥ 1.25**，在 `GOTOOLCHAIN=local` 下会直接构建失败（或诱导升级工具链，ADR-007 禁止）。
  升级前必须先解决工具链问题，不得静默 bump。
- `modernc.org/sqlite` 锁在 v1.46.1，理由见 ADR-007。

## 字体许可要点（为什么可以内嵌进二进制并再分发）

`golang.org/x/image/font/gofont/ttfs/README` 原文是 BSD 三条款（版权方 Bigelow & Holmes，为 Go 项目定制），
其中第二条要求：**以二进制形式再分发时，必须在随附文档中复制该版权声明与条件**。本文件即履行该义务。

它**不是** OFL。DESIGN §8.2 的"素材与语料必须本地自有或许可证允许再分发（OFL 字体 / CC0 图片）"这条约束仍然有效，
只是本字体走的是 BSD 路径：允许再分发，条件是把声明写在这里。

## 未使用

- 任何远程图片、外部生成 API、在线模型、Python/Pillow/ImageMagick 服务端依赖：**都没有使用**（AGENTS 硬性约束）。
- `golang.org/x/image/font/basicfont`、`draw.CatmullRom`、`webp/tiff/bmp` 等子包：未 import（只有 x/text 因 sfnt 的编码表被间接拉入）。
- 题库、语料、图片素材：阶段 3B **没有任何外部素材**，图片全部由代码实时生成。

## 复核方式

```bash
go list -m all                      # 直接 + 传递依赖与版本
grep -h '^require\|^\t' go.mod      # 直接依赖只有 x/image 与 modernc.org/sqlite
go list -deps ./cmd/server | grep 'golang.org/x'   # 服务二进制实际链接了什么
```
