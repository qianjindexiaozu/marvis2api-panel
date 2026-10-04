package account

import (
	"os"
	"syscall"
)

// syscallEBUSY bind mount rename 回退判据（独立小文件便于测试替换）。
var syscallEBUSY = syscall.EBUSY

// writeFileDirect 原地写入（O_TRUNC，权限 0600）+ fsync。
func writeFileDirect(fp string, raw []byte) error {
	f, err := os.OpenFile(fp, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(raw)
	if werr == nil {
		werr = f.Sync()
	}
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
