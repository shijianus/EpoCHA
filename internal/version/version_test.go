package version

import (
	"strings"
	"testing"
)

func TestDefaultsAreNonEmpty(t *testing.T) {
	for name, v := range map[string]string{"Version": Version, "Commit": Commit, "BuildDate": BuildDate} {
		if strings.TrimSpace(v) == "" {
			t.Errorf("%s 为空：ldflags 注入路径断裂", name)
		}
	}
	if Version == "0.0.0-dev" && Commit == "none" {
		t.Log("未注入构建信息的开发构建（正常，make build 会注入）")
	}
}
