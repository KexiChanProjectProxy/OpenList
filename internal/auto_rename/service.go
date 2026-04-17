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
		if s.AutoRename {
			if err := a.startWatcherLocked(s.MountPath); err != nil {
				log.Warnf("auto_rename failed to start watcher for %s: %+v", s.MountPath, err)
			}
		}
	}
	return nil
}

func (a *AutoRenameService) startWatcher(mountPath string) error {
	watcher, err := NewWatcher(mountPath)
	if err != nil {
		return err
	}
	if err := watcher.Start(); err != nil {
		return err
	}
	a.watchers[mountPath] = watcher
	log.Infof("auto_rename started watching: %s", mountPath)
	return nil
}

func (a *AutoRenameService) stopWatcher(mountPath string) {
	if w, ok := a.watchers[mountPath]; ok {
		w.Stop()
		delete(a.watchers, mountPath)
		log.Infof("auto_rename stopped watching: %s", mountPath)
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

func (a *AutoRenameService) OnStorageChanged(typ string, storage driver.Driver) {
	a.mu.Lock()
	defer a.mu.Unlock()

	s := storage.GetStorage()
	switch typ {
	case "add", "update":
		if s.AutoRename && !s.Disabled {
			if _, exists := a.watchers[s.MountPath]; !exists {
				if err := a.startWatcherLocked(s.MountPath); err != nil {
					log.Warnf("auto_rename failed to start watcher for %s: %+v", s.MountPath, err)
				}
			}
		} else {
			a.stopWatcherLocked(s.MountPath)
		}
	case "del":
		a.stopWatcherLocked(s.MountPath)
	}
}

func (a *AutoRenameService) startWatcherLocked(mountPath string) error {
	watcher, err := NewWatcher(mountPath)
	if err != nil {
		return err
	}
	if err := watcher.Start(); err != nil {
		return err
	}
	a.watchers[mountPath] = watcher
	log.Infof("auto_rename started watching: %s", mountPath)
	return nil
}

func (a *AutoRenameService) stopWatcherLocked(mountPath string) {
	if w, ok := a.watchers[mountPath]; ok {
		w.Stop()
		delete(a.watchers, mountPath)
		log.Infof("auto_rename stopped watching: %s", mountPath)
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
}
