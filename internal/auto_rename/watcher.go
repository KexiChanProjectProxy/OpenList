package auto_rename

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

const (
	EventDelay         = 3 * time.Second
	FileStableChecks   = 4
	FileStableInterval = time.Second
)

var imageExts = map[string]struct{}{
	".jpg":  {},
	".jpeg": {},
	".png":  {},
	".webp": {},
	".gif":  {},
	".bmp":  {},
	".tiff": {},
	".heic": {},
	".heif": {},
}

type dirWorker struct {
	ch chan string
}

type Watcher struct {
	root string

	watcher *fsnotify.Watcher
	done    chan struct{}

	mu      sync.Mutex
	started bool

	pending map[string]*time.Timer
	workers map[string]*dirWorker
}

func NewWatcher(root string) (*Watcher, error) {
	root = filepath.Clean(root)
	if root == "" || root == "." {
		return nil, fmt.Errorf("invalid root path: %q", root)
	}
	return &Watcher{
		root:    root,
		pending: make(map[string]*time.Timer),
		workers: make(map[string]*dirWorker),
	}, nil
}

func (w *Watcher) Start() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return nil
	}
	if err := w.ensureRoot(); err != nil {
		return err
	}
	ws, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.watcher = ws
	w.done = make(chan struct{})
	done := w.done
	if err := w.watcher.Add(w.root); err != nil {
		_ = w.watcher.Close()
		w.watcher = nil
		return err
	}
	if err := w.addExistingFirstLevelDirsLocked(); err != nil {
		_ = w.watcher.Close()
		w.watcher = nil
		return err
	}
	w.started = true
	go w.loopEvents(done, ws)
	log.Infof("auto_rename watcher started, root: %s", w.root)
	return nil
}

func (w *Watcher) Stop() {
	w.mu.Lock()
	if !w.started {
		w.mu.Unlock()
		return
	}
	w.started = false
	if w.done != nil {
		close(w.done)
	}
	for _, t := range w.pending {
		t.Stop()
	}
	w.pending = make(map[string]*time.Timer)
	for dir := range w.workers {
		delete(w.workers, dir)
	}
	w.workers = make(map[string]*dirWorker)
	if w.watcher != nil {
		_ = w.watcher.Close()
		w.watcher = nil
	}
	w.mu.Unlock()
	log.Infof("auto_rename watcher stopped")
}

func (w *Watcher) ensureRoot() error {
	st, err := os.Stat(w.root)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("root path is not a directory: %s", w.root)
	}
	return nil
}

func (w *Watcher) addExistingFirstLevelDirsLocked() error {
	entries, err := os.ReadDir(w.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirPath := filepath.Join(w.root, entry.Name())
		if err := w.watcher.Add(dirPath); err != nil {
			log.Warnf("auto_rename add dir watch failed: %s, err: %+v", dirPath, err)
			continue
		}
		log.Debugf("auto_rename watching dir: %s", dirPath)
	}
	return nil
}

func (w *Watcher) loopEvents(done <-chan struct{}, ws *fsnotify.Watcher) {
	for {
		select {
		case <-done:
			return
		case event, ok := <-ws.Events:
			if !ok {
				return
			}
			w.handleEvent(event)
		case err, ok := <-ws.Errors:
			if !ok {
				return
			}
			log.Errorf("auto_rename watcher error: %+v", err)
		}
	}
}

func (w *Watcher) handleEvent(event fsnotify.Event) {
	if event.Name == "" {
		return
	}
	p := filepath.Clean(event.Name)
	if p == w.root {
		return
	}
	rel, err := filepath.Rel(w.root, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 1 {
		w.handleRootLevelEvent(p, event)
		return
	}
	if len(parts) != 2 {
		return
	}
	if !isImageFile(p) {
		return
	}
	if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) == 0 {
		return
	}
	w.schedule(p)
}

func (w *Watcher) handleRootLevelEvent(path string, event fsnotify.Event) {
	if event.Op&fsnotify.Create != 0 {
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			w.mu.Lock()
			if w.watcher != nil {
				if err := w.watcher.Add(path); err != nil {
					log.Warnf("auto_rename add new dir watch failed: %s, err: %+v", path, err)
				} else {
					log.Debugf("auto_rename watching new dir: %s", path)
				}
			}
			w.mu.Unlock()
		}
	}
	if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		w.mu.Lock()
		delete(w.workers, path)
		w.mu.Unlock()
	}
}

func (w *Watcher) schedule(filePath string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.pending[filePath]; ok {
		w.pending[filePath].Reset(EventDelay)
		return
	}
	w.pending[filePath] = time.AfterFunc(EventDelay, func() {
		w.handleScheduledFile(filePath)
	})
}

func (w *Watcher) handleScheduledFile(filePath string) {
	w.mu.Lock()
	delete(w.pending, filePath)
	w.mu.Unlock()

	stable, err := waitFileStable(filePath, FileStableChecks, FileStableInterval)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Warnf("auto_rename stable check failed: %s, err: %+v", filePath, err)
		}
		return
	}
	if !stable {
		return
	}

	dir := filepath.Dir(filePath)
	w.enqueue(dir, filePath)
}

func (w *Watcher) enqueue(dir, filePath string) {
	worker := w.getOrCreateDirWorker(dir)
	w.mu.Lock()
	done := w.done
	w.mu.Unlock()
	select {
	case worker.ch <- filePath:
	case <-done:
	}
}

func (w *Watcher) getOrCreateDirWorker(dir string) *dirWorker {
	w.mu.Lock()
	defer w.mu.Unlock()
	if worker, ok := w.workers[dir]; ok {
		return worker
	}
	worker := &dirWorker{ch: make(chan string, 128)}
	done := w.done
	w.workers[dir] = worker
	go func() {
		for {
			select {
			case <-done:
				return
			case filePath := <-worker.ch:
				if err := renameFile(filePath); err != nil {
					if !errors.Is(err, os.ErrNotExist) {
						log.Warnf("auto_rename rename failed: %s, err: %+v", filePath, err)
					}
				}
			}
		}
	}()
	return worker
}

func isImageFile(filePath string) bool {
	ext := strings.ToLower(filepath.Ext(filePath))
	_, ok := imageExts[ext]
	return ok
}

func waitFileStable(filePath string, checks int, interval time.Duration) (bool, error) {
	var (
		lastSize    int64
		lastModTime time.Time
		haveLast    bool
		stableCount int
	)
	for i := 0; i < checks*3; i++ {
		st, err := os.Stat(filePath)
		if err != nil {
			return false, err
		}
		if st.IsDir() {
			return false, nil
		}
		size := st.Size()
		mod := st.ModTime()
		if haveLast && size == lastSize && mod.Equal(lastModTime) {
			stableCount++
			if stableCount >= checks {
				return true, nil
			}
		} else {
			stableCount = 1
			haveLast = true
			lastSize = size
			lastModTime = mod
		}
		time.Sleep(interval)
	}
	return false, nil
}

func renameFile(filePath string) error {
	st, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return nil
	}
	dir := filepath.Dir(filePath)
	baseDir := filepath.Base(dir)
	ext := filepath.Ext(filePath)
	date := time.Now().Format("20060102")
	prefix := fmt.Sprintf("%s%s-", baseDir, date)

	for seq := 1; ; seq++ {
		newName := fmt.Sprintf("%s%03d%s", prefix, seq, ext)
		target := filepath.Join(dir, newName)
		if filepath.Clean(target) == filepath.Clean(filePath) {
			return nil
		}
		if _, err := os.Stat(target); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(filePath, target); err != nil {
			return err
		}
		log.Infof("auto_rename renamed: %s -> %s", filePath, target)
		return nil
	}
}
