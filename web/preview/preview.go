// Package preview 内嵌本地预览页面。
//
// 它是**开发工具**（cmd/preview 唯一使用者），不是阶段 4 的正式 widget：
// 单文件原生 HTML/CSS/JS，零依赖、零构建步骤。放在 web/preview 而不是 cmd/preview
// 是因为 go:embed 只能嵌入本包子树，而页面不该和 main 混在一个目录里（DESIGN §15）。
package preview

//go:generate echo "页面就是同目录下的 index.html"

import _ "embed"

// IndexHTML 是预览页原文，其中的 {{CFG_JSON}} 由演示后端注入**公开**配置。
//
//go:embed index.html
var IndexHTML []byte

// ConfigPlaceholder 是页面里的注入点。
const ConfigPlaceholder = "{{CFG_JSON}}"
