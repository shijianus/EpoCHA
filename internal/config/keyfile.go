package config

// 阶段 5A 的密钥注入面：每一把密钥材料都可以改由**文件**提供（同名变量 + _FILE）。
// 存在的理由不是"多一种写法"，而是容器里 env 的泄露面：`ps`、`docker inspect`、
// 编排平台的 KV 与 CI 日志都会原样带出环境变量，而 bind mount / secret 文件不会。
// 这一条把"密钥只在文件的 0600 权限里"变成可执行配置，而不是文档里的一句劝告。

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// secretEntry 解析 "<NAME>" 与 "<NAME>_FILE" 这一对入口，返回值与"值来自哪一路"。
//
// 两者同时非空即拒绝，**不做优先级**：env 与挂载文件在真实部署里常由不同的人、
// 在不同的时间点配置，"文件优先"或"env 优先"都会制造"运维改的那一份不生效"这种
// 极难定位的故障；而密钥配错的失败面是签名与摘要，静默选一份比拒绝启动危险得多。
// 空串（变量存在但为空）与"变量不存在"同样按未设置处理，与 os.Getenv 语义一致。
func secretEntry(getenv func(string) string, envName, fileEnvName string) (val, source string, err error) {
	v := strings.TrimSpace(getenv(envName))
	p := strings.TrimSpace(getenv(fileEnvName))
	switch {
	case v != "" && p != "":
		return "", "", fmt.Errorf("%w: 同时设置了 %s 与 %s，只能保留一个（不静默优先）", ErrInvalid, envName, fileEnvName)
	case v != "":
		return v, sourceEnv, nil
	case p != "":
		b, err := readKeyFile(p)
		if err != nil {
			// 文案只带入口变量名与失败类别：文件内容不进文案（它本身就是密钥），
			// 路径也不进（路径会原样出现在启动日志里）。
			return "", "", fmt.Errorf("%w: %s %s", ErrInvalid, fileEnvName, err)
		}
		return string(b), sourceFile, nil
	default:
		return "", "", nil
	}
}

// readKeyFile 读取一个密钥文件。每条不合法都返回错误而不是"当成空值"：
// fail closed 的价值就在于配错的那一次启动直接失败，而不是带着错误密钥对外服务。
func readKeyFile(path string) ([]byte, error) {
	// 用 Stat 而不是 Lstat：/run/secrets/<name> 与编排平台的 secret 挂载**本身就是符号链接**
	// （指向 ..data/<uuid>，为原子轮换设计）。拒绝符号链接等于否掉这个入口的主要用法，
	// 所以校验落在解析后的目标上：目标必须是普通文件，且目标自身权限受限。
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		return nil, errors.New("指向的文件不存在或不可读")
	case !fi.Mode().IsRegular():
		return nil, errors.New("指向的不是普通文件")
	case fi.Mode().Perm()&0o077 != 0:
		return nil, errors.New("权限过宽，group/other 位必须为空（0600 或 0400）")
	case fi.Size() == 0:
		return nil, errors.New("文件为空")
	case fi.Size() > maxKeyFileBytes:
		return nil, fmt.Errorf("文件超过 %d 字节上限", maxKeyFileBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("指向的文件不存在或不可读")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return nil, errors.New("读取失败")
	}
	if len(b) > maxKeyFileBytes {
		return nil, fmt.Errorf("文件超过 %d 字节上限", maxKeyFileBytes)
	}
	// 首尾空白忽略（编辑器或 echo 留下的换行），**内部**字节原样保留；
	// 与 env 路径上的 TrimSpace 行为一致，否则同一个值经两条入口会得到两个结论。
	if b = bytes.TrimSpace(b); len(b) == 0 {
		return nil, errors.New("文件内容只有空白")
	}
	return b, nil
}

// LoadPepper 按与 server 完全相同的规则装载 pepper（含 ≥32 字节与 base64 校验）。
//
// cmd/ops（备份/恢复/容器内管理）与 server 必须按**同一套**规则读密钥：
// 两处各写一份"env 还是文件、谁优先、什么算不合法"，迟早会出现两套真相，
// 而配错的失败面是签名与摘要——那正是拒绝启动也优于静默选择的原因。
func LoadPepper(getenv func(string) string) ([]byte, error) {
	k, _, err := readKey(getenv, envPepper, envPepperFile)
	return k, err
}

// LoadAdminKey 按与 server 完全相同的规则装载管理凭据（逐字节，不做 base64 变换）。
func LoadAdminKey(getenv func(string) string) ([]byte, error) {
	k, _, err := readAdminKey(getenv)
	return k, err
}

// entryName 只用于错误文案：点名**实际被设置的那一个**入口变量。
func entryName(envName, fileEnvName, source string) string {
	if source == sourceFile {
		return fileEnvName
	}
	return envName
}
