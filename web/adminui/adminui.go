// Package adminui 内嵌管理控制台的静态页面：原生 HTML/CSS/JS，零框架、零构建步骤。
//
// 两条结构性事实决定了这个包怎么写：
//
//  1. 它**只**应挂在管理监听器上。公共监听器没有这些路由，所以"公网碰不到管理面"
//     由网络边界保证，而不是由页面里有没有鉴权保证；
//  2. 页面里的管理凭据只活在 JavaScript 的模块变量里（见 app.js），后端依旧对每个
//     /api/admin/* 请求做 Bearer 校验 —— 登录界面**不是**鉴权，它只是把凭据递给后端。
//
// 资源名与 URL 都是**白名单常量**：不存在"按请求参数拼路径"，因此目录穿越与枚举在结构上
// 不可能（测试仍然探一遍——结构不可能不等于没被测过）。
package adminui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
)

//go:embed index.html app.css app.js
var assets embed.FS

// 资源名（embed 内的键）与对外 URL。
const (
	AssetHTML = "index.html"
	AssetCSS  = "app.css"
	AssetJS   = "app.js"

	URLIndex = "/admin"
	URLCSS   = "/admin/app.css"
	URLJS    = "/admin/app.js"
)

// CSP：没有 unsafe-eval，也没有 unsafe-inline —— 脚本与样式一律来自同源外部文件，
// 服务端文本只经 textContent 落地，页面里不存在 innerHTML 注入点。
// frame-ancestors 'none' 与下面的 X-Frame-Options 是一对（后者管老浏览器）。
// form-action 'none'：页面自己不发原生表单提交，提交全部走 fetch；
// 万一脚本没跑起来，这条让"表单被提交到别处"失败关闭而不是静默发生。
const csp = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self'; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'none'; " +
	"base-uri 'none'; " +
	"object-src 'none'; " +
	"frame-ancestors 'none'"

// ErrUnknownAsset 是"要的资源名不在白名单里"。
var ErrUnknownAsset = errors.New("adminui: 未知资源")

// AssetBytes 读出内嵌资源。
func AssetBytes(name string) ([]byte, error) {
	switch name {
	case AssetHTML, AssetCSS, AssetJS:
	default:
		return nil, ErrUnknownAsset
	}
	b, err := assets.ReadFile(name)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.New("adminui: 内嵌资源为空")
	}
	return b, nil
}

// ETag 由内容哈希得来：不需要手工维护版本号。
func ETag(name string) string {
	b, err := AssetBytes(name)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// Handler 返回管理页面的只读静态路由。未知路径一律 404：没有目录、不读磁盘、
// 也不因为"管理监听器只有运维能碰到"就放开任意文件读取。
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+URLIndex, serveIndex)
	// "/admin/" 注册的是**子树**模式，所以处理器自己必须把非根路径退回 404，
	// 否则 /admin/anything 都会拿到页面（那不是"好看的重定向"，那是枚举面）。
	mux.HandleFunc("GET "+URLIndex+"/", serveIndex)
	mux.HandleFunc("GET "+URLJS, func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, r, AssetJS, "text/javascript; charset=utf-8")
	})
	mux.HandleFunc("GET "+URLCSS, func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, r, AssetCSS, "text/css; charset=utf-8")
	})
	return mux
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != URLIndex && r.URL.Path != URLIndex+"/" {
		http.NotFound(w, r)
		return
	}
	writeAsset(w, r, AssetHTML, "text/html; charset=utf-8")
}

// writeAsset 用**我们自己声明的** Content-Type 写出去，不走 http.ServeContent：
// 精简容器里没有 /etc/mime.types 时 ServeContent 会嗅探成 text/plain，配 nosniff
// 浏览器就拒绝执行脚本（这条是 widget 实测踩到的，见 web/widget/widget.go）。
//
// no-store 是刻意的：管理页面没有按身份变化的内容，但一份过期的 app.js 会让运维
// 看到与后端不符的字段。省下的那点字节不值得这个歧义。
func writeAsset(w http.ResponseWriter, r *http.Request, name, contentType string) {
	body, err := AssetBytes(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", csp)
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("ETag", ETag(name))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func serveAsset(w http.ResponseWriter, r *http.Request, name, contentType string) {
	writeAsset(w, r, name, contentType)
}
