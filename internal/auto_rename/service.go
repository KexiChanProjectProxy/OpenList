package auto_rename

import (
	"sync"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	log "github.com/sirupsen/logrus"
)

var Service = &AutoRenameService{}

type AutoRenameService struct {
	mu       sync.RWMutex
	watchers map[string]*Watcher
}

func (a *AutoRenameService) Name() string {
	return "auto_rename"
}

// resolveLocalPath returns the real local filesystem path for a storage driver.
// It checks if the driver implements IRootPath (e.g. local driver) and uses that;
// otherwise falls back to MountPath.
func resolveLocalPath(storageDriver driver.Driver) string {
	if rp, ok := storageDriver.(driver.IRootPath); ok {
		return rp.GetRootPath()
	}
	return storageDriver.GetStorage().MountPath
}

func (a *AutoRenameService) Init() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, w := range a.watchers {
		w.Stop()
	}
	a.watchers = make(map[string]*Watcher)

	storages, err := db.GetEnabledStorages()
	if err != nil {
		log.Errorf("auto_rename failed to get storages: %+v", err)
		return err
	}

	for _, s := range storages {
		if !s.AutoRename {
			continue
		}
		storageDriver, err := op.GetStorageByMountPath(s.MountPath)
		if err != nil {
			log.Warnf("auto_rename failed to get storage driver for %s: %+v", s.MountPath, err)
			continue
		}
		localPath := resolveLocalPath(storageDriver)
		if localPath == "" {
			log.Warnf("auto_rename skipped storage %s: no local path resolved", s.MountPath)
			continue
		}
		if err := a.startWatcherLocked(localPath); err != nil {
			log.Warnf("auto_rename failed to start watcher for %s (local: %s): %+v", s.MountPath, localPath, err)
		}
	}
	return nil
}

func (a *AutoRenameService) startWatcher(localPath string) error {
	watcher, err := NewWatcher(localPath)
	if err != nil {
		return err
	}
	if err := watcher.Start(); err != nil {
		return err
	}
	a.watchers[localPath] = watcher
	log.Infof("auto_rename started watching: %s", localPath)
	return nil
}

func (a *AutoRenameService) stopWatcher(localPath string) {
	if w, ok := a.watchers[localPath]; ok {
		w.Stop()
		delete(a.watchers, localPath)
		log.Infof("auto_rename stopped watching: %s", localPath)
	}
}

func (a *AutoRenameService) IsReady() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.watchers) >= 0
}

func (a *AutoRenameService) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, w := range a.watchers {
		w.Stop()
	}
	a.watchers = make(map[string]*Watcher)
}

func (a *AutoRenameService) OnStorageChanged(typ string, storageDriver driver.Driver) {
	a.mu.Lock()
	defer a.mu.Unlock()

	s := storageDriver.GetStorage()
	localPath := resolveLocalPath(storageDriver)

	switch typ {
	case "add", "update":
		if s.AutoRename && !s.Disabled {
			if localPath == "" {
				log.Warnf("auto_rename skipped storage %s: no local path resolved", s.MountPath)
				return
			}
			if _, exists := a.watchers[localPath]; !exists {
				if err := a.startWatcherLocked(localPath); err != nil {
					log.Warnf("auto_rename failed to start watcher for %s (local: %s): %+v", s.MountPath, localPath, err)
				}
			}
		} else {
			a.stopWatcherLocked(localPath)
		}
	case "del":
		a.stopWatcherLocked(localPath)
	}
}

func (a *AutoRenameService) startWatcherLocked(localPath string) error {
	watcher, err := NewWatcher(localPath)
	if err != nil {
		return err
	}
	if err := watcher.Start(); err != nil {
		return err
	}
	a.watchers[localPath] = watcher
	log.Infof("auto_rename started watching: %s", localPath)
	return nil
}

func (a *AutoRenameService) stopWatcherLocked(localPath string) {
	if w, ok := a.watchers[localPath]; ok {
		w.Stop()
		delete(a.watchers, localPath)
		log.Infof("auto_rename stopped watching: %s", localPath)
	}
}

func init() {
	Service.watchers = make(map[string]*Watcher)

	op.RegisterStorageHook(func(typ string, storage driver.Driver) {
		Service.OnStorageChanged(typ, storage)
	})

	op.RegisterSettingChangingCallback(func() {
		if err := Service.Init(); err != nil {
			log.Errorf("auto_rename Init failed: %+v", err)
		}
	})

	op.RegisterStoragesLoadedCallback(func() {
		if err := Service.Init(); err != nil {
			log.Errorf("auto_rename Init on startup failed: %+v", err)
		}
	})
}
