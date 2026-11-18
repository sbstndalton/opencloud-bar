package main

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// The Transfers page: what is moving right now. Kept folders come from rclone's progress
// blocks (activity.go); the drive's upload queue comes from its remote control. Read-only.

// setTransfers remembers the files a folder's running sync reports (nil clears them).
func (e *Engine) setTransfers(key string, list []Transfer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(list) == 0 {
		delete(e.transfers, key)
		return
	}
	if e.transfers == nil {
		e.transfers = map[string][]Transfer{}
	}
	e.transfers[key] = list
}

// folderTransfer is a Transfer as the page shows it.
type folderTransfer struct {
	Transfer
	FolderName string `json:"folderName"`
}

// DriveUpload is one file of the drive's upload queue.
type DriveUpload struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Uploading bool   `json:"uploading"`
}

// driveQueueMax is how many queued files the page lists; the counts cover all of them.
const driveQueueMax = 100

// driveQueueEvery is the least time between two asks of the drive's rclone, however often the page asks.
const driveQueueEvery = 5 * time.Second

type driveQueueCache struct {
	mu   sync.Mutex
	at   time.Time
	data map[string]any
}

// transfersForPage is what GET /ui/api/transfers answers.
func (e *Engine) transfersForPage() map[string]any {
	e.mu.Lock()
	var folders []folderTransfer
	for key, list := range e.transfers {
		name := key
		if a := e.active[key]; a != nil && a.name != "" {
			name = a.name
		}
		for _, t := range list {
			folders = append(folders, folderTransfer{Transfer: t, FolderName: name})
		}
	}
	e.mu.Unlock()
	sort.SliceStable(folders, func(i, j int) bool { return folders[i].FolderName < folders[j].FolderName })
	if folders == nil {
		folders = []folderTransfer{}
	}
	return map[string]any{"folders": folders, "drive": e.driveQueue()}
}

// driveQueue reads the drive's upload queue, at most once per driveQueueEvery. rclone's
// `vfs/queue` lists each file (rclone.org/rc/#vfs-queue; the bundled rclone v1.76 lists it in
// rc/list); a rclone without it still answers `vfs/stats`, whose diskCache counts are used.
func (e *Engine) driveQueue() map[string]any {
	e.mu.Lock()
	running := e.mount != nil
	e.mu.Unlock()
	if !running {
		return map[string]any{"mounted": false}
	}
	c := &e.driveQ
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data != nil && time.Since(c.at) < driveQueueEvery {
		return c.data
	}
	c.data, c.at = e.readDriveQueue(), time.Now()
	return c.data
}

func (e *Engine) readDriveQueue() map[string]any {
	if out, err := e.rc("vfs/queue", 5*time.Second); err == nil {
		var v struct {
			Queue []DriveUpload `json:"queue"`
		}
		if json.Unmarshal(out, &v) == nil {
			inProgress := 0
			for _, u := range v.Queue {
				if u.Uploading {
					inProgress++
				}
			}
			files := v.Queue
			if len(files) > driveQueueMax {
				files = files[:driveQueueMax]
			}
			if files == nil {
				files = []DriveUpload{}
			}
			return map[string]any{"mounted": true, "files": files, "uploadsInProgress": inProgress,
				"uploadsQueued": len(v.Queue) - inProgress}
		}
	}
	out, err := e.rc("vfs/stats", 5*time.Second)
	if err != nil {
		return map[string]any{"mounted": true, "error": "rclone didn't answer"}
	}
	var v struct {
		DiskCache *struct {
			UploadsInProgress int `json:"uploadsInProgress"`
			UploadsQueued     int `json:"uploadsQueued"`
		} `json:"diskCache"`
	}
	if json.Unmarshal(out, &v) != nil || v.DiskCache == nil {
		return map[string]any{"mounted": true, "error": "couldn't read rclone's answer"}
	}
	// No file names from this rclone: only how many.
	return map[string]any{"mounted": true, "files": []DriveUpload{}, "countsOnly": true,
		"uploadsInProgress": v.DiskCache.UploadsInProgress, "uploadsQueued": v.DiskCache.UploadsQueued}
}
