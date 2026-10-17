package widget

// 生产 widget 的静态资源：内嵌进单二进制（DESIGN D10），由公共监听器以版本化 URL 提供
// （§10.1 的 GET /widget/v1.js）。没有第三方运行时，构建期也不需要任何打包工具。

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

//go:embed v1.js v1.worker.js
var assets embed.FS

// 资源名与 URL 都是**白名单常量**：不存在"按请求参数拼路径"这件事，
// 所以目录穿越与枚举在结构上不可能（测试仍然探一遍——结构不可能不等于没被测过）。
const (
	JSPath       = "v1.js"
	WorkerPath   = "v1.worker.js"
	URLJS        = "/widget/v1.js"
	URLWorker    = "/widget/v1.worker.js"
	contentType  = "text/javascript; charset=utf-8"
	cacheControl = "public, max-age=3600"
)

// ErrUnknownAsset 是"要的资源名不在白名单里"。
var ErrUnknownAsset = errors.New("widget: 未知资源")

// AssetBytes 读出内嵌资源。
func AssetBytes(name string) ([]byte, error) {
	switch name {
	case JSPath, WorkerPath:
	default:
		return nil, ErrUnknownAsset
	}
	b, err := assets.ReadFile(name)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.New("widget: 内嵌资源为空")
	}
	return b, nil
}

// ETag 由内容哈希得来：改一版脚本，条件请求自然失效，不需要手工维护版本号。
func ETag(name string) string {
	b, err := AssetBytes(name)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// Handler 返回只读静态资源的处理器。未知路径一律 404，绝不回退到"列目录"或读磁盘。
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+URLJS, func(w http.ResponseWriter, r *http.Request) {
		writeAsset(w, r, JSPath)
	})
	mux.HandleFunc("GET "+URLWorker, func(w http.ResponseWriter, r *http.Request) {
		writeAsset(w, r, WorkerPath)
	})
	return mux
}

// writeAsset 用**我们自己声明的** Content-Type 写出去，不走 http.ServeContent。
// ServeContent 在 `mime.TypeByExtension` 查不到扩展名时（精简容器里没有 /etc/mime.types 就是这样）
// 会嗅探内容并把响应头覆盖成 text/plain；配 nosniff，浏览器就拒绝把脚本当脚本执行。
// 这条是实测踩到的：页面报 "blocked due to MIME type (text/plain)"，
// 而在装了 mime.types 的开发机上跑测试却一切正常——所以这里不依赖系统 mime 表。
//
// 不支持 Range 是刻意的：脚本没有分段下载需求，支持它就要自己处理 206/416 边界。
func writeAsset(w http.ResponseWriter, r *http.Request, name string) {
	body, err := AssetBytes(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	// 公共中间件给所有响应打了 no-store；脚本必须可缓存，否则每次领题都回源一次。
	// 覆盖它不是放宽安全策略：这份响应里没有任何按身份变化的内容。
	h.Set("Cache-Control", cacheControl)
	h.Del("Pragma")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Type", contentType)
	h.Set("Accept-Ranges", "none")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if et := ETag(name); et != "" {
		h.Set("ETag", et)
		if r.Header.Get("If-None-Match") == et {
			h.Del("Content-Length")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// extractTemplateLiteral 取出 v1.js 里 String.raw`…` 包住的那段 worker 源码。
// 只做一件事：把"内嵌副本"还原成字节，好与 v1.worker.js 逐字节比对（防两份拷贝分叉）。
func extractTemplateLiteral(src, marker string) (string, error) {
	i := strings.Index(src, marker)
	if i < 0 {
		return "", errors.New("widget: 找不到内嵌标记 " + marker)
	}
	rest := src[i+len(marker):]
	open := strings.Index(rest, "`")
	if open < 0 {
		return "", errors.New("widget: 内嵌标记后没有模板字符串起点")
	}
	closeIdx := strings.Index(rest[open+1:], "`")
	if closeIdx < 0 {
		return "", errors.New("widget: 内嵌模板字符串没有终点")
	}
	return rest[open+1 : open+1+closeIdx], nil
}
