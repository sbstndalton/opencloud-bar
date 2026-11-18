package main

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// The Transfers page: what is moving right now. Kept folders come from rclone's progress
// blocks (activity.go); the drive's upload queue comes from its remote control. Read-only.

// setTransfers remembers the files a folder's running sync reports (nil clears them). A folder
// that isn't syncing any more is ignored: a late report can't bring its files back.
func (e *Engine) setTransfers(key string, list []Transfer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(list) == 0 {
		delete(e.transfers, key)
		return
	}
	if e.active[key] == nil {
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

// driveQueueCache holds the last answer, a failure too (also for driveQueueEvery, so a rclone
// that hangs isn't asked again and again). mu guards only the fields (never held across an rc
// call); `fetching` is the single-flight guard: callers that arrive during a fetch wait for it
// and share its answer.
type driveQueueCache struct {
	mu       sync.Mutex
	at       time.Time
	data     map[string]any
	fetching chan struct{} // non-nil while a fetch runs; closed when it is done
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

// driveQueue reads the drive's upload queue, at most once per driveQueueEvery (a failed read counts, and
// concurrent callers share one read). rclone's
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
	for {
		c.mu.Lock()
		if c.data != nil && time.Since(c.at) < driveQueueEvery {
			d := c.data
			c.mu.Unlock()
			return d
		}
		if wait := c.fetching; wait != nil {
			c.mu.Unlock()
			<-wait // someone is asking rclone; use their answer
			c.mu.Lock()
			d := c.data
			c.mu.Unlock()
			if d != nil {
				return d
			}
			continue
		}
		done := make(chan struct{})
		c.fetching = done
		c.mu.Unlock()
		d := e.readDriveQueue() // no lock held while rclone answers (or doesn't)
		c.mu.Lock()
		c.data, c.at, c.fetching = d, time.Now(), nil
		c.mu.Unlock()
		close(done)
		return d
	}
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
