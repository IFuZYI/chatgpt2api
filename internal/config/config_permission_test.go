package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 设置保存失败时，错误信息必须给出**可操作的修复指引**。
//
// 真实场景（Docker 单文件 bind mount）：
//
//	宿主 config.json 是 root:root 644，容器以 uid 65532 运行
//	→ os.WriteFile 创建 .tmp 失败（/app 目录不可写）
//	→ 回退就地写也失败（文件属主不是 65532）
//	→ 用户只看到 "open /app/config.json: permission denied"
//
// 这句话没有告诉用户任何东西：是文件权限？目录权限？还是挂载问题？
// 该怎么修？错误信息必须回答这些。
func TestUpdatePermissionErrorIsActionable(t *testing.T) {
	// 直接测错误包装函数（不依赖能否真的构造只读场景，
	// root 环境下 chmod 无效，靠注入错误来验证包装逻辑）
	cause := os.ErrPermission
	wrapped := explainConfigWriteError("/app/config.json", cause)
	if wrapped == nil {
		t.Fatal("包装后返回 nil")
	}

	msg := wrapped.Error()
	for _, want := range []string{
		"/app/config.json", // 哪个文件
		"chown",            // 怎么修（宿主侧命令）
		"65532",            // 改成哪个 uid
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息缺 %q，用户无法自助修复：%s", want, msg)
		}
	}
}

// 非权限类错误不能被误包装成权限提示。
func TestUpdateOtherErrorsPassThrough(t *testing.T) {
	other := os.ErrNotExist
	wrapped := explainConfigWriteError("/app/config.json", other)
	if wrapped == nil {
		t.Fatal("返回 nil")
	}
	if strings.Contains(wrapped.Error(), "chown") {
		t.Errorf("非权限错误被误包装成权限提示: %v", wrapped)
	}
}

// 权限错误包装后仍必须可被 errors.Is 识别为 ErrPermission，
// 否则上层的错误分类（例如映射到 403 还是 500）会失效。
func TestWrappedPermissionErrorKeepsIdentity(t *testing.T) {
	wrapped := explainConfigWriteError(filepath.Join("x", "config.json"), os.ErrPermission)
	if !isPermissionError(wrapped) {
		t.Error("包装后丢失了 permission 语义")
	}
}
