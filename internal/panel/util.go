// 面板包内部小工具：限长读取。
package panel

import (
	"io"
)

// readAllLimit 有上限读取。
func readAllLimit(r io.Reader, limit int64) []byte {
	raw, _ := io.ReadAll(io.LimitReader(r, limit))
	return raw
}
