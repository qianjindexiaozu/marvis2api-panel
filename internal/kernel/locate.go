package kernel

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

var (
	errNoBridge  = errors.New("Host 还没有连上登录桥")
	errNotReady  = errors.New("账号内核尚未就绪")
	errNoInstall = errors.New("本机没有安装 Marvis，无法为每个账号启动独立内核")
	errNoAccount = errors.New("没有可用账号：请在面板扫码添加")
)

func installed() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := os.Stat(hostBin())
	return err == nil
}

func hostBin() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library/Application Support/com.tencent.mac.marvis/components/MarvisGateway/Current/MarvisHost")
}

func agentBin() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library/Application Support/com.tencent.mac.marvis/components/MarvisAgent/Current/MarvisAgent")
}

func beaconLib() string {
	p := "/Applications/Marvis.app/Contents/Resources/bin/libbeacon_wrapper.dylib"
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}
