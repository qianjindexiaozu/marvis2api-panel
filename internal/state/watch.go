// 凭证文件热加载：周期比对 auth.json 的修改时刻，变了就重新读入（免重启）。
//
// 轮询而非 fsnotify——零依赖、跨平台行为一致（fsnotify 不在标准库内，且网络盘
// / NFS 上事件不可靠）。面板保存走即时路径，不经此。
package state

import (
	"log"
	"sync"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
)

// watchInterval 文件扫描周期（热加载粒度）。
const watchInterval = 10 * time.Second

// Watcher 凭证文件热加载器。
type Watcher struct {
	st    *State
	file  *account.File
	every time.Duration

	mu       sync.Mutex
	lastMod  time.Time
	lastSize int64

	stopCh chan struct{}
	once   sync.Once
}

// StartWatch 启动热加载（interval <= 0 用默认）。
func StartWatch(st *State, file *account.File, interval time.Duration) *Watcher {
	if interval <= 0 {
		interval = watchInterval
	}
	w := &Watcher{st: st, file: file, every: interval, stopCh: make(chan struct{})}
	w.snapshot()
	go w.loop()
	return w
}

// Stop 停止扫描（幂等）。
func (w *Watcher) Stop() { w.once.Do(func() { close(w.stopCh) }) }

func (w *Watcher) loop() {
	t := time.NewTicker(w.every)
	defer t.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-t.C:
			w.round()
		}
	}
}

// snapshot 记录当前文件指纹，避免启动第一轮把已有凭证误报为「新增」。
func (w *Watcher) snapshot() {
	w.mu.Lock()
	defer w.mu.Unlock()
	mod, size, err := w.file.Stat()
	if err != nil {
		return
	}
	w.lastMod, w.lastSize = mod, size
}

func (w *Watcher) round() {
	mod, size, err := w.file.Stat()
	if err != nil {
		return // 文件不存在/不可读：保持现状
	}
	w.mu.Lock()
	same := mod.Equal(w.lastMod) && size == w.lastSize
	w.lastMod, w.lastSize = mod, size
	w.mu.Unlock()
	if same {
		return
	}
	a, err := w.file.Load()
	if err != nil {
		log.Printf("WARN: [watch] 凭证读取失败，保留现有凭证: %v", err)
		return
	}
	if a == nil {
		log.Printf("[watch] 凭证文件已清空/删除：网关转为未配置状态")
		w.st.SetAuth(nil)
		return
	}
	if changed := w.st.SetAuth(a); changed {
		log.Printf("[watch] 凭证已热加载：%s", a.Label())
	}
}
