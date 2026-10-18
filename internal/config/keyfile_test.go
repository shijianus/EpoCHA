package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 密钥文件入口（EPOCHA_*_FILE）的验收：fail closed、两套真相拒绝、内容不外泄。
// 这一组测试存在的原因是"部署时密钥从文件读"是阶段 5 的唯一安全密钥注入方式，
// 而它最危险的失败模式恰好是**静默回落**：文件读不到就用默认值、env 与文件同时存在时随便选一个。

func writeKeyFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "keyfile")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func b64Key(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// baseEnvFile 用文件提供 master/pepper，其余沿用 baseEnv 的默认值。
func baseEnvFile(t *testing.T, master, pepper string) map[string]string {
	t.Helper()
	return map[string]string{
		envMasterKeyFile: writeKeyFile(t, master, 0o600),
		envPepperFile:    writeKeyFile(t, pepper, 0o600),
		envDB:            filepath.Join(t.TempDir(), "epocha.db"),
	}
}

func TestLoadKeysFromFiles(t *testing.T) {
	env := baseEnvFile(t, b64Key(sentinelKey)+"\n", b64Key(sentinelPepper))
	c, err := Load(nil, envMap(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(c.MasterKey) != sentinelKey {
		t.Errorf("主密钥装载不符（长度 %d）", len(c.MasterKey))
	}
	if string(c.Pepper) != sentinelPepper {
		t.Errorf("pepper 长度 = %d，期望 %d", len(c.Pepper), len(sentinelPepper))
	}
	if c.MasterKeySource != "file" || c.PepperSource != "file" {
		t.Errorf("密钥来源标注 = %q/%q", c.MasterKeySource, c.PepperSource)
	}
	if !strings.Contains(c.SafeSummary(), "master_key_source=file") {
		t.Errorf("SafeSummary 未标注来源：%s", c.SafeSummary())
	}
}

func TestEnvAndFileBothSetIsRefused(t *testing.T) {
	// 每一对都必须拒绝：静默选一个会让运维以为改的是实际生效的那一份。
	// 被试的这一对里 env 与 file **都**给合法值，其余密钥走 env 保证 Load 能走到这一步。
	cases := []struct {
		name         string
		envName      string
		fileEnvName  string
		masterViaEnv bool
		admin        bool
		value        string
		fileValue    string
	}{
		{name: "master", envName: envMasterKey, fileEnvName: envMasterKeyFile, value: b64Key(sentinelKey), fileValue: b64Key(sentinelKey)},
		{name: "pepper", envName: envPepper, fileEnvName: envPepperFile, value: b64Key(sentinelPepper), fileValue: b64Key(sentinelPepper)},
		{name: "previous", envName: envPrevMasterKey, fileEnvName: envPrevMasterKeyFile, value: b64Key(sentinelKey), fileValue: b64Key(sentinelKey)},
		{name: "admin", envName: envAdminKey, fileEnvName: envAdminKeyFile, value: "ak_" + strings.Repeat("c", 64), fileValue: "ak_" + strings.Repeat("c", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{
				envMasterKey: b64Key(sentinelKey),
				envPepper:    b64Key(sentinelPepper),
				envDB:        filepath.Join(t.TempDir(), "epocha.db"),
			}
			env[tc.envName] = tc.value
			env[tc.fileEnvName] = writeKeyFile(t, tc.fileValue, 0o600)
			_, err := Load(nil, envMap(env))
			if err == nil {
				t.Fatalf("%s 与 %s 同时设置却成功了", tc.envName, tc.fileEnvName)
			}
			if !errorsIsInvalid(err) {
				t.Errorf("错误族不符: %v", err)
			}
			for _, want := range []string{tc.envName, tc.fileEnvName} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误文案未点名 %s: %v", want, err)
				}
			}
		})
	}
}

func TestAdminKeyFromEnvStillWinsAlone(t *testing.T) {
	// 单独设 env（不设 file）必须仍按原语义工作——这条防止我把新逻辑写成"必须有文件"。
	env := baseEnvFile(t, b64Key(sentinelKey), b64Key(sentinelPepper))
	env[envAdminKey] = "ak_" + strings.Repeat("a", 64)
	c, err := Load(nil, envMap(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(c.AdminKey) != env[envAdminKey] {
		t.Error("env 管理凭据未按原样装载")
	}
	if c.AdminKeySource != "env" {
		t.Errorf("来源 = %q", c.AdminKeySource)
	}
}

func TestKeyFileMissingOrUnusableRefuses(t *testing.T) {
	good := b64Key(sentinelPepper)
	dir := t.TempDir()
	symlinkTarget := writeKeyFile(t, good, 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(symlinkTarget, link); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "notregular")
	if err := os.WriteFile(fifo, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		file string
		mode os.FileMode
	}{
		{"不存在", filepath.Join(dir, "nope"), 0},
		{"是目录", nested, 0},
		{"group 可读", symlinkTarget, 0o640},
		{"other 可读", symlinkTarget, 0o604},
		{"world 可写", symlinkTarget, 0o606},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{
				envMasterKey:  b64Key(sentinelKey),
				envPepperFile: tc.file,
				envDB:         filepath.Join(t.TempDir(), "epocha.db"),
			}
			if tc.mode != 0 {
				p := writeKeyFile(t, good, tc.mode)
				env[envPepperFile] = p
			}
			_, err := Load(nil, envMap(env))
			if err == nil {
				t.Fatalf("%s 时仍然启动成功（fail closed 失效）", tc.name)
			}
			if !strings.Contains(err.Error(), envPepperFile) {
				t.Errorf("错误文案未点名 %s: %v", envPepperFile, err)
			}
		})
	}

	t.Run("符号链接可用（/run/secrets 形态）", func(t *testing.T) {
		env := map[string]string{
			envMasterKey:  b64Key(sentinelKey),
			envPepperFile: link,
			envDB:         filepath.Join(t.TempDir(), "epocha.db"),
		}
		if _, err := Load(nil, envMap(env)); err != nil {
			t.Fatalf("符号链接被拒: %v", err)
		}
	})
}

func TestKeyFileContentInvalid(t *testing.T) {
	tooBig := b64Key(sentinelPepper) + "|" + strings.Repeat("x", maxKeyFileBytes)
	cases := []struct {
		name  string
		value string
	}{
		{"空文件", ""},
		{"只有换行", "\n"},
		{"不是 base64", "这不是 base64!!!"},
		{"解码后不足 32 字节", b64Key("short")},
		{"超过大小上限", tooBig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{
				envMasterKey:  b64Key(sentinelKey),
				envPepperFile: writeKeyFile(t, tc.value, 0o600),
				envDB:         filepath.Join(t.TempDir(), "epocha.db"),
			}
			_, err := Load(nil, envMap(env))
			if err == nil {
				t.Fatalf("%s 时仍然成功", tc.name)
			}
			// 失败文案绝不回显文件内容——base64 值本身就是密钥材料。
			if strings.Contains(err.Error(), tc.value) && tc.value != "" {
				t.Errorf("错误文案回显了密钥内容: %v", err)
			}
		})
	}
}

func TestAdminKeyFileIsVerbatimBytes(t *testing.T) {
	// 管理凭据不做 base64 变换：文件里写的就是运维发出的 Bearer 值（DESIGN §6）。
	raw := "ak_" + strings.Repeat("b", 64)
	env := baseEnvFile(t, b64Key(sentinelKey), b64Key(sentinelPepper))
	env[envAdminKeyFile] = writeKeyFile(t, raw+"\n", 0o600)
	c, err := Load(nil, envMap(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(c.AdminKey) != raw {
		t.Errorf("管理凭据 = %q，与文件内容不符（尾部换行应被忽略，其余按字节）", string(c.AdminKey))
	}
	if c.AdminKeySource != "file" {
		t.Errorf("来源 = %q", c.AdminKeySource)
	}
	if strings.Contains(c.SafeSummary(), raw) {
		t.Error("SafeSummary 泄漏了管理凭据")
	}

	t.Run("短于 32 字符仍拒绝", func(t *testing.T) {
		env := baseEnvFile(t, b64Key(sentinelKey), b64Key(sentinelPepper))
		env[envAdminKeyFile] = writeKeyFile(t, "ak_short", 0o600)
		if _, err := Load(nil, envMap(env)); err == nil {
			t.Fatal("过短的管理凭据被接受")
		}
	})
	t.Run("含空白仍拒绝", func(t *testing.T) {
		env := baseEnvFile(t, b64Key(sentinelKey), b64Key(sentinelPepper))
		env[envAdminKeyFile] = writeKeyFile(t, "ak_with space_inside_and_length_enough_1234567890", 0o600)
		if _, err := Load(nil, envMap(env)); err == nil {
			t.Fatal("含控制字符的管理凭据被接受")
		}
	})
}

func TestPreviousMasterFileOnly(t *testing.T) {
	// 轮换期间 current 走 env、previous 走文件是完全合法的组合。
	env := map[string]string{
		envMasterKey:         b64Key(sentinelKey),
		envPepper:            b64Key(sentinelPepper),
		envPrevMasterKeyFile: writeKeyFile(t, b64Key("previous-master-key-material-0123456789"), 0o600),
		envDB:                filepath.Join(t.TempDir(), "epocha.db"),
	}
	c, err := Load(nil, envMap(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(c.PrevMaster) != "previous-master-key-material-0123456789" {
		t.Errorf("previous 密钥未按文件装载（长度 %d）", len(c.PrevMaster))
	}
	if c.MasterKeySource != "env" {
		t.Errorf("来源标注 = %q", c.MasterKeySource)
	}
}

// errorsIsInvalid 只判断错误族，不判断文案，避免测试与措辞绑死。
func errorsIsInvalid(err error) bool { return strings.Contains(fmt.Sprint(err), "config:") }

// 运维命令（cmd/ops）与 server 共用同一套密钥入口：这一组钉住导出的两个装载器
// 没有变成"第二套规则"（尤其不许出现"文件读不到就回落默认值"）。
func TestLoadPepperForOpsMatchesServerRules(t *testing.T) {
	t.Run("env 合法", func(t *testing.T) {
		k, err := LoadPepper(envMap(map[string]string{envPepper: b64Key(sentinelPepper)}))
		if err != nil {
			t.Fatalf("LoadPepper: %v", err)
		}
		if string(k) != sentinelPepper {
			t.Errorf("pepper 长度 = %d，期望 %d", len(k), len(sentinelPepper))
		}
	})
	t.Run("file 合法", func(t *testing.T) {
		k, err := LoadPepper(envMap(map[string]string{
			envPepperFile: writeKeyFile(t, b64Key(sentinelPepper)+"\n", 0o600),
		}))
		if err != nil {
			t.Fatalf("LoadPepper: %v", err)
		}
		if string(k) != sentinelPepper {
			t.Error("文件入口的 pepper 内容不符")
		}
	})
	t.Run("两者同时设置拒绝", func(t *testing.T) {
		_, err := LoadPepper(envMap(map[string]string{
			envPepper:     b64Key(sentinelPepper),
			envPepperFile: writeKeyFile(t, b64Key(sentinelPepper), 0o600),
		}))
		if err == nil {
			t.Fatal("同时设置仍成功")
		}
	})
	t.Run("都没设置是缺失而不是默认值", func(t *testing.T) {
		_, err := LoadPepper(envMap(map[string]string{}))
		if !errors.Is(err, ErrMissingPepper) {
			t.Fatalf("err = %v，期望 ErrMissingPepper（内置默认密钥等于没有密钥）", err)
		}
	})
	t.Run("权限过宽的文件拒绝且不回显内容", func(t *testing.T) {
		p := writeKeyFile(t, b64Key(sentinelPepper), 0o644)
		_, err := LoadPepper(envMap(map[string]string{envPepperFile: p}))
		if err == nil {
			t.Fatal("group/other 可读的密钥文件被接受")
		}
		if strings.Contains(err.Error(), b64Key(sentinelPepper)) {
			t.Errorf("错误文案回显了密钥内容: %v", err)
		}
	})
}

func TestLoadAdminKeyForOpsMatchesServerRules(t *testing.T) {
	raw := "ak_" + strings.Repeat("d", 64)
	k, err := LoadAdminKey(envMap(map[string]string{envAdminKey: raw}))
	if err != nil {
		t.Fatalf("LoadAdminKey: %v", err)
	}
	if string(k) != raw {
		t.Error("管理凭据必须逐字节装载（不做 base64 变换）")
	}
	if _, err := LoadAdminKey(envMap(map[string]string{})); err == nil {
		t.Error("未设置管理凭据却返回成功")
	}
	if _, err := LoadAdminKey(envMap(map[string]string{envAdminKeyFile: writeKeyFile(t, "ak_short", 0o600)})); err == nil {
		t.Error("过短的管理凭据被接受")
	}
}
