package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	persistInterval = 5 * time.Minute
	persistFileName = "usage-stats.json"
)

var (
	persistPath   atomic.Value
	persistOnce   sync.Once
	persistStopCh chan struct{}
)

// EnablePersistence loads existing stats from disk and starts periodic saving.
// The dir parameter specifies the directory where the usage-stats.json file will be stored.
func EnablePersistence(dir string) {
	if dir == "" {
		return
	}

	path := filepath.Join(dir, persistFileName)
	persistPath.Store(path)

	if err := loadFromDisk(path); err != nil {
		log.WithError(err).Warn("failed to load persisted usage statistics")
	} else if _, statErr := os.Stat(path); statErr == nil {
		log.Infof("usage statistics loaded from %s", path)
	}

	persistOnce.Do(func() {
		persistStopCh = make(chan struct{})
		go runPeriodicSave()
	})
}

// StopPersistence performs a final save and stops the periodic saver.
func StopPersistence() {
	if persistStopCh != nil {
		close(persistStopCh)
	}
}

func loadFromDisk(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}

	var snapshot StatisticsSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}

	result := defaultRequestStatistics.MergeSnapshot(snapshot)
	log.Debugf("usage persistence: merged %d records, skipped %d", result.Added, result.Skipped)
	return nil
}

// SaveToDisk persists the current statistics snapshot to disk.
func SaveToDisk() error {
	path, ok := persistPath.Load().(string)
	if !ok || path == "" {
		return nil
	}

	snapshot := defaultRequestStatistics.Snapshot()
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func runPeriodicSave() {
	ticker := time.NewTicker(persistInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := SaveToDisk(); err != nil {
				log.WithError(err).Warn("failed to persist usage statistics")
			}
		case <-persistStopCh:
			if err := SaveToDisk(); err != nil {
				log.WithError(err).Warn("failed to persist usage statistics on shutdown")
			}
			return
		}
	}
}
