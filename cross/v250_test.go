package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// 2.5: the Transfers page.

// Real rclone v1.76 output (rclone copy --disable Copy --bwlimit 1M --transfers 2 --stats 1s
// --stats-log-level NOTICE --stats-file-name-length 0): a block with two files in flight, one
// with a name that holds ", ", ": " and "(45%)"; then a block with one file at 100% and no ETA
// yet; a normal log line; and a last block with nothing in flight.
const transferBlocks = "2026/11/18 02:23:32 NOTICE: \n" +
	"Transferred:   \t    1.992 MiB / 4.768 MiB, 42%, 1.992 MiB/s, ETA 1s\n" +
	"Checks:                 0 / 0, -, Listed 3\n" +
	"Transferred:            0 / 2, 0%\n" +
	"Elapsed time:         1.0s\n" +
	"Transferring:\n" +
	" * b.bin: 52% / 1.907 MiB, 1019.975 KiB/s, 0s\n" +
	" * sub dir/big, file: v1 (45%).bin: 34% / 2.861 MiB, 1019.990 KiB/s, 1s\n" +
	"\n" +
	"2026/11/18 02:23:35 NOTICE: \n" +
	"Transferred:   \t    4.768 MiB / 4.768 MiB, 100%, 1.192 MiB/s, ETA 0s\n" +
	"Transferred:            1 / 2, 50%\n" +
	"Transferring:\n" +
	" * sub dir/big, file: v1 (45%).bin:100% / 2.861 MiB, 732.420 KiB/s, -\n" +
	"\n" +
	"2026/11/18 02:23:35 NOTICE: f.txt: Can't follow symlink without -L/--copy-links\n" +
	"2026/11/18 02:23:36 NOTICE: \n" +
	"Transferred:            2 / 2, 100%\n" +
	"Elapsed time:         4.7s\n" +
	"\n"

func TestStatsFilterTransfers(t *testing.T) {
	var log bytes.Buffer
	var got [][]Transfer
	sf := &statsFilter{w: &log, folder: "Docs", onTransfers: func(l []Transfer) { got = append(got, l) }}
	for i := 0; i < len(transferBlocks); i += 11 { // arrives in pieces, split anywhere
		_, _ = sf.Write([]byte(transferBlocks[i:min(i+11, len(transferBlocks))]))
	}
	sf.Flush()
	if len(got) != 3 || len(got[0]) != 2 || len(got[1]) != 1 || len(got[2]) != 0 {
		t.Fatalf("blocks: %+v", got)
	}
	want := []Transfer{
		{Folder: "Docs", Name: "b.bin", Percent: 52, Size: "1.907 MiB", Speed: "1019.975 KiB/s", ETA: "0s"},
		{Folder: "Docs", Name: "sub dir/big, file: v1 (45%).bin", Percent: 34, Size: "2.861 MiB", Speed: "1019.990 KiB/s", ETA: "1s"},
	}
	for i := range want {
		if got[0][i] != want[i] {
			t.Errorf("transfer %d: %+v, want %+v", i, got[0][i], want[i])
		}
	}
	if x := got[1][0]; x.Percent != 100 || x.ETA != "" || x.Name != "sub dir/big, file: v1 (45%).bin" {
		t.Errorf("100%% with no ETA: %+v", x)
	}
	if s := log.String(); s != "2026/11/18 02:23:35 NOTICE: f.txt: Can't follow symlink without -L/--copy-links\n" {
		t.Errorf("progress blocks leaked into the log: %q", s)
	}
}

func TestParseTransfer(t *testing.T) {
	for line, want := range map[string]Transfer{
		" *  path/name.ext: 45% /10Mi, 1Mi/s, 5s":             {Name: "path/name.ext", Percent: 45, Size: "10Mi", Speed: "1Mi/s", ETA: "5s"},
		" * name.ext:  0% / 10 MiB, 0 B/s, -":                 {Name: "name.ext", Percent: 0, Size: "10 MiB", Speed: "0 B/s"},
		" * name.ext:100% / 0 B, 0 B/s, -":                    {Name: "name.ext", Percent: 100, Size: "0 B", Speed: "0 B/s"},
		" * a: b/c.txt: 7% / 1 KiB, 2 B/s, 1h2m3s":            {Name: "a: b/c.txt", Percent: 7, Size: "1 KiB", Speed: "2 B/s", ETA: "1h2m3s"},
		" * name: transferring":                               {Name: "name", Percent: -1},
		" *  dir/some file.txt: transferring":                 {Name: "dir/some file.txt", Percent: -1},
		" * Überraschung — 日本語.txt: 12% / 3 MiB, 1 MiB/s, 2s": {Name: "Überraschung — 日本語.txt", Percent: 12, Size: "3 MiB", Speed: "1 MiB/s", ETA: "2s"},
	} {
		got, ok := parseTransfer(line)
		if !ok || got != want {
			t.Errorf("%q: %+v %v, want %+v", line, got, ok, want)
		}
	}
	for _, bad := range []string{"", "Transferring:", "Checks:  1 / 1, 100%", " * : 5% / 1 B, 1 B/s, 1s", "no star: 5% / 1 B, 1 B/s, 1s", " * name"} {
		if got, ok := parseTransfer(bad); ok {
			t.Errorf("%q parsed: %+v", bad, got)
		}
	}
}

// A fake rc: answers by path; a missing path is rclone's 404.
type fakeRc struct {
	mu    sync.Mutex
	hits  map[string]int
	reply map[string]string
}

func rcRig(t *testing.T, reply map[string]string) (*Engine, *fakeRc) {
	t.Helper()
	e := pausedRig(t, "")
	f := &fakeRc{hits: map[string]int{}, reply: reply}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits[strings.TrimPrefix(r.URL.Path, "/")]++
		body, ok := f.reply[strings.TrimPrefix(r.URL.Path, "/")]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	var port int
	_, _ = fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port)
	e.cfg.LocalPort = port - 100 // RcPort is LocalPort + 100
	e.mount = &exec.Cmd{}        // a drive "running"
	return e, f
}

func (f *fakeRc) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func TestDriveQueueListsFiles(t *testing.T) {
	e, rc := rcRig(t, map[string]string{
		// vfs/queue as rclone answers it: https://rclone.org/rc/#vfs-queue
		"vfs/queue": `{"queue":[{"name":"a/one.bin","id":1,"size":2048,"expiry":0,"tries":1,"delay":0,"uploading":true},
			{"name":"two.txt","id":2,"size":10,"expiry":4.5,"tries":0,"delay":5,"uploading":false}]}`,
		"vfs/stats": `{"diskCache":{"uploadsInProgress":9,"uploadsQueued":9}}`,
	})
	d := e.driveQueue()
	files, _ := d["files"].([]DriveUpload)
	if d["mounted"] != true || len(files) != 2 || files[0] != (DriveUpload{Name: "a/one.bin", Size: 2048, Uploading: true}) ||
		d["uploadsInProgress"] != 1 || d["uploadsQueued"] != 1 || d["countsOnly"] != nil {
		t.Fatalf("queue: %+v", d)
	}
	if rc.count("vfs/stats") != 0 {
		t.Error("vfs/stats asked although vfs/queue answered")
	}
	// Asked again at once (the page polls): answered from memory; after 5 s, asked again.
	_ = e.driveQueue()
	if n := rc.count("vfs/queue"); n != 1 {
		t.Errorf("rc asked %d times within 5 s", n)
	}
	e.driveQ.at = time.Now().Add(-driveQueueEvery - time.Second)
	_ = e.driveQueue()
	if n := rc.count("vfs/queue"); n != 2 {
		t.Errorf("rc asked %d times after 5 s", n)
	}
}

func TestDriveQueueFallsBackToStats(t *testing.T) {
	e, rc := rcRig(t, map[string]string{"vfs/stats": `{"diskCache":{"bytesUsed":5,"uploadsInProgress":2,"uploadsQueued":3}}`})
	d := e.driveQueue()
	if files, _ := d["files"].([]DriveUpload); d["countsOnly"] != true || len(files) != 0 || d["uploadsInProgress"] != 2 || d["uploadsQueued"] != 3 {
		t.Fatalf("fallback: %+v", d)
	}
	if rc.count("vfs/queue") != 1 || rc.count("vfs/stats") != 1 {
		t.Errorf("calls: %v", rc.hits)
	}
}

func TestDriveQueueErrorsAndLimits(t *testing.T) {
	e, _ := rcRig(t, map[string]string{})
	if d := e.driveQueue(); d["error"] == nil || d["mounted"] != true {
		t.Errorf("rc without answers: %+v", d)
	}
	var q []string
	for i := 0; i < driveQueueMax+5; i++ {
		q = append(q, fmt.Sprintf(`{"name":"f%d","size":1,"uploading":false}`, i))
	}
	e, _ = rcRig(t, map[string]string{"vfs/queue": `{"queue":[` + strings.Join(q, ",") + `]}`})
	d := e.driveQueue()
	if files, _ := d["files"].([]DriveUpload); len(files) != driveQueueMax || d["uploadsQueued"] != driveQueueMax+5 {
		t.Errorf("limit: %d files, %v queued", len(files), d["uploadsQueued"])
	}
	// A drive that isn't running is not asked at all.
	e = pausedRig(t, "")
	if d := e.driveQueue(); d["mounted"] != false {
		t.Errorf("no drive: %+v", d)
	}
}

func TestTransfersAPI(t *testing.T) {
	r := newUIRig(t)
	if resp, _ := r.do("GET", "/ui/api/transfers", "", "", nil); resp.StatusCode != 401 {
		t.Errorf("transfers without the page's login: %d", resp.StatusCode)
	}
	r.login()
	type answer struct {
		Folders []map[string]any `json:"folders"`
		Drive   map[string]any   `json:"drive"`
	}
	get := func() answer {
		resp, body := r.do("GET", "/ui/api/transfers", "", "", nil)
		var a answer
		if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &a) != nil {
			t.Fatalf("transfers: %d %s", resp.StatusCode, body)
		}
		return a
	}
	if a := get(); a.Folders == nil || len(a.Folders) != 0 || a.Drive["mounted"] != false {
		t.Errorf("empty: %+v", a)
	}
	r.e.mu.Lock()
	r.e.active["z"] = &activeRun{name: "Zebra"}
	r.e.active["a"] = &activeRun{name: "Alpha"}
	r.e.mu.Unlock()
	r.e.setTransfers("z", []Transfer{{Folder: "z", Name: "z.bin", Percent: 3, Size: "1 MiB", Speed: "1 MiB/s", ETA: "1s"}})
	r.e.setTransfers("a", []Transfer{{Folder: "a", Name: "<b>x</b>.bin", Percent: -1}})
	a := get()
	if len(a.Folders) != 2 || a.Folders[0]["folderName"] != "Alpha" || a.Folders[0]["name"] != "<b>x</b>.bin" ||
		a.Folders[0]["percent"] != float64(-1) || a.Folders[1]["folderName"] != "Zebra" || a.Folders[1]["eta"] != "1s" {
		t.Errorf("folders: %+v", a.Folders)
	}
	r.e.setTransfers("z", nil)
	if a := get(); len(a.Folders) != 1 {
		t.Errorf("cleared folder still listed: %+v", a.Folders)
	}
}
