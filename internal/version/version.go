// Package version 提供构建信息，由 Makefile 通过 -ldflags 注入。
package version

// Version 是语义化版本号，dev 表示未发布构建。
var Version = "0.0.0-dev"

// Commit 是构建来源的 git short SHA。
var Commit = "none"

// BuildDate 是构建时间（RFC3339）。
var BuildDate = "unknown"
