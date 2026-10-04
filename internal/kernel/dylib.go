package kernel

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

//go:embed redirect_src.txt
var redirectSrc embed.FS

const sandboxProfile = `(version 1)
(allow default)
(deny file-write* file-read* (literal "/tmp/marvisgateway_kb"))
(deny file-write* file-read* (literal "/tmp/marvisagent_k9Xm2P7vB4nQ8wLY"))
`

var dylibOnce sync.Mutex

// compileRedirect 把路径重写库编译到缓存目录。官方 Host 允许注入动态库，
// 所以不必改它的签名。
func compileRedirect(cacheDir string) (string, error) {
	dylibOnce.Lock()
	defer dylibOnce.Unlock()
	src, err := redirectSrc.ReadFile("redirect_src.txt")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(src)
	dst := filepath.Join(cacheDir, "redirect-"+hex.EncodeToString(sum[:8])+".dylib")
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", err
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		return "", fmt.Errorf("编译 IPC 重定向库需要 cc（Xcode 命令行工具）")
	}
	srcPath := dst + ".c"
	if err := os.WriteFile(srcPath, src, 0o600); err != nil {
		return "", err
	}
	cmd := exec.Command(cc, "-dynamiclib", "-o", dst, srcPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("编译 IPC 重定向库失败: %v: %s", err, out)
	}
	return dst, nil
}
