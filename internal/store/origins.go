package store

// 允许来源（Origin）的规范化与精确匹配（DESIGN §5.3、§10.3）。
//
// 三条硬规则，都是为了让"后缀相似"这类混淆无处落脚：
//  1. 按 scheme + hostname + **有效端口** 规范化后**整串精确比较**，绝不做字符串后缀匹配；
//  2. 第一版不支持通配符，也不自动放行子域——example.com 不涵盖 app.example.com；
//  3. `null`、空、畸形一律拒绝。
//
// Origin 是浏览器强制设置、脚本改写的头，但它对**非浏览器**攻击者可自由伪造：
// 白名单挡的是"别的站点/别的页面拿你的 site_key 做题"，不是"机器人 vs 真人"。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	MaxOriginLen   = 253
	MaxHostLen     = 253
	CanonicalHTTPS = "https"
	CanonicalHTTP  = "http"
)

// NormalizeOrigin 把浏览器给的 Origin 串规范成"有效来源"字符串。
// 规则：只允许 http/https；无用户信息；无路径/查询/片段（只允许单独的 "/"）；
// 主机名小写化，IPv6 保留方括号形式；默认端口不写入结果。
func NormalizeOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%w: Origin 为空", ErrBadConfig)
	}
	if len(raw) > MaxOriginLen {
		return "", fmt.Errorf("%w: Origin 超过 %d 字符", ErrBadConfig, MaxOriginLen)
	}
	// "null" 是沙箱 iframe / file: 场景浏览器给出的值，它不表达任何可校验的站点身份。
	if strings.EqualFold(raw, "null") {
		return "", fmt.Errorf("%w: 不允许 null Origin", ErrBadConfig)
	}
	if strings.ContainsAny(raw, "*") {
		return "", fmt.Errorf("%w: 第一版不支持通配符来源", ErrBadConfig)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: Origin 无法解析", ErrBadConfig)
	}
	if u.Scheme != CanonicalHTTP && u.Scheme != CanonicalHTTPS {
		return "", fmt.Errorf("%w: 只允许 http/https，got %q", ErrBadConfig, u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: Origin 不得含用户信息", ErrBadConfig)
	}
	if p := strings.TrimRight(u.Path, "/"); p != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: Origin 不得含路径/查询/片段", ErrBadConfig)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: Origin 缺少主机名", ErrBadConfig)
	}

	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if host == "" {
		return "", fmt.Errorf("%w: Origin 主机名为空", ErrBadConfig)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			// IPv6 字面量必须带方括号才能与端口区分；规范化成标准文本形式
			host = "[" + strings.Trim(ip.String(), "[]") + "]"
		} else {
			host = ip.String()
		}
	} else {
		if len(host) > MaxHostLen {
			return "", fmt.Errorf("%w: 主机名超过 %d 字符", ErrBadConfig, MaxHostLen)
		}
		// DNS 标签校验：不允许空标签、前导/尾随点、控制字符或空格
		if strings.HasSuffix(host, ".") {
			host = strings.TrimSuffix(host, ".")
		}
		for _, lbl := range strings.Split(host, ".") {
			if lbl == "" || !labelOK(lbl) {
				return "", fmt.Errorf("%w: 主机名 %q 含非法标签", ErrBadConfig, host)
			}
		}
		if !strings.Contains(host, ".") {
			// 裸主机名（"localhost"、单标签）在生产里没有可判定的站点身份；
			// 只有 loopback 显式放行（开发环境用），其余拒绝。
			if host != "localhost" {
				return "", fmt.Errorf("%w: 单标签主机名只允许 localhost", ErrBadConfig)
			}
		}
	}

	defPort := "443"
	if u.Scheme == CanonicalHTTP {
		defPort = "80"
	}
	if port == "" {
		port = defPort
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%w: Origin 端口非法", ErrBadConfig)
	}
	if port == defPort {
		port = "" // 默认端口不写入规范形式，使 https://x 与 https://x:443 视为同一个来源
	}
	out := u.Scheme + "://" + host
	if port != "" {
		out += ":" + port
	}
	if len(out) > MaxOriginLen {
		return "", fmt.Errorf("%w: 规范化后的 Origin 过长", ErrBadConfig)
	}
	return out, nil
}

func labelOK(lbl string) bool {
	if len(lbl) > 63 {
		return false
	}
	for i, r := range lbl {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-':
			if i == 0 || i == len(lbl)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// OriginHost 从规范化 Origin 里取出主机部分，供 /siteverify 返回 hostname。
func OriginHost(canonical string) string {
	if canonical == "" {
		return ""
	}
	u, err := url.Parse(canonical)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// ReplaceOrigins 全量替换站点的允许来源。空列表是合法状态："该站点不接受任何浏览器来源"，
// 此时只有 allow_missing_origin 放行的直接 API 调用能签发。
func (d *DB) ReplaceOrigins(ctx context.Context, siteID int64, origins []string) ([]string, error) {
	if _, err := d.SiteByID(ctx, siteID); err != nil {
		return nil, err
	}
	clean, err := normalizeOrigins(origins)
	if err != nil {
		return nil, err
	}
	tx, err := d.h.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: 来源事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM site_origins WHERE site_id=?`, siteID); err != nil {
		return nil, fmt.Errorf("store: 清空来源: %w", err)
	}
	if err := insertOrigins(ctx, tx, siteID, clean, formatTime(time.Now().UTC())); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: 提交来源: %w", err)
	}
	return d.Origins(ctx, siteID)
}

func normalizeOrigins(origins []string) ([]string, error) {
	if len(origins) > MaxOriginsPerSite {
		return nil, fmt.Errorf("%w: 允许来源上限为 %d", ErrBadConfig, MaxOriginsPerSite)
	}
	clean := make([]string, 0, len(origins))
	seen := map[string]bool{}
	for _, o := range origins {
		n, err := NormalizeOrigin(o)
		if err != nil {
			return nil, err
		}
		if !seen[n] {
			seen[n] = true
			clean = append(clean, n)
		}
	}
	return clean, nil
}

func insertOrigins(ctx context.Context, tx *sql.Tx, siteID int64, clean []string, now string) error {
	for _, o := range clean {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO site_origins(site_id, origin, created_at) VALUES(?,?,?)`, siteID, o, now); err != nil {
			return fmt.Errorf("store: 写入来源: %w", err)
		}
	}
	return nil
}

// Origins 返回站点的允许来源列表。
func (d *DB) Origins(ctx context.Context, siteID int64) ([]string, error) {
	rows, err := d.h.QueryContext(ctx,
		`SELECT origin FROM site_origins WHERE site_id=? ORDER BY origin`, siteID)
	if err != nil {
		return nil, fmt.Errorf("store: 列来源: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			return nil, fmt.Errorf("store: 扫描来源: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OriginAllowed 精确匹配一条规范化后的来源。
func (d *DB) OriginAllowed(ctx context.Context, siteID int64, canonical string) (bool, error) {
	var n int
	err := d.h.QueryRowContext(ctx,
		`SELECT count(*) FROM site_origins WHERE site_id=? AND origin=?`, siteID, canonical).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: 查来源: %w", err)
	}
	return n > 0, nil
}

// ErrBadOrigin 供调用方区分"来源被白名单拒绝"与其他形态错误。
var ErrBadOrigin = errors.New("store: 来源不在允许列表")
