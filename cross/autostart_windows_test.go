//go:build windows

package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

func TestAutostartRegistryRoundTrip(t *testing.T) {
	testEnv(t)
	// A key of its own under HKCU, never the real Run key.
	sub := fmt.Sprintf(`Software\OpenCloudBarTest-%d`, os.Getpid())
	old := autostartRunKey
	autostartRunKey = sub
	t.Cleanup(func() {
		autostartRunKey = old
		_ = registry.DeleteKey(registry.CURRENT_USER, sub)
	})

	if osAutostart().Enabled {
		t.Fatal("enabled before anything was written")
	}
	if err := osSetAutostart(false); err != nil { // no key at all: not an error
		t.Fatal(err)
	}
	if err := osSetAutostart(true); err != nil {
		t.Fatal(err)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, sub, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := k.GetStringValue(installerRunValueName(t)) // the name the installer writes, from its script
	k.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v, `"`) || !strings.HasSuffix(v, `.exe"`) {
		t.Errorf("value %q isn't a quoted exe path", v)
	}
	if st := osAutostart(); !st.Supported || !st.Enabled {
		t.Errorf("after enabling: %+v", st)
	}
	if err := osSetAutostart(false); err != nil {
		t.Fatal(err)
	}
	if osAutostart().Enabled {
		t.Error("still enabled after removing")
	}
	if err := osSetAutostart(false); err != nil {
		t.Error(err)
	}
}
