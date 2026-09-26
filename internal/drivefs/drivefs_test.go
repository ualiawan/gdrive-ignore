package drivefs

import (
	"encoding/hex"
	"errors"
	"testing"
)

func TestParseSyncTargets(t *testing.T) {
	// Captured from a real install: account 100000000000000000001 on G:.
	b, _ := hex.DecodeString("0A1D0A170A153130303030303030303030303030303030303030311202473A")
	got := parseSyncTargets(b)
	if len(got) != 1 || got[0].account != "100000000000000000001" || got[0].mount != "G:" {
		t.Fatalf("got %+v", got)
	}
	if got := parseSyncTargets([]byte{0x0A, 0xFF}); len(got) != 0 {
		t.Fatalf("malformed input: %+v", got)
	}
}

func TestCovering(t *testing.T) {
	info := Info{Locations: []Location{
		{Kind: KindStream, Path: `G:\My Drive`},
		{Kind: KindBackup, Path: `C:\Users\me\Backup`},
	}}
	if l := info.Covering(`c:\users\me\backup\proj`); l == nil || l.Kind != KindBackup {
		t.Errorf("backup not covering: %+v", l)
	}
	if l := info.Covering(`C:\Users\me\BackupX`); l != nil {
		t.Errorf("prefix match without separator: %+v", l)
	}
	if !info.IsLocationRoot(`g:\my drive\`) {
		t.Error("root not detected")
	}
}

// TestDiscoverLive only logs; it documents what this machine reports.
func TestDiscoverLive(t *testing.T) {
	info := Discover()
	t.Logf("installed=%v running=%v warnings=%v", info.Installed, info.Running, info.Warnings)
	for _, l := range info.Locations {
		t.Logf("  %-6s %-40s %s exists=%v", l.Kind, l.Path, l.Label, l.Exists)
	}
}

// TestItemsLive reads this machine's Drive databases (read-only) if present.
func TestItemsLive(t *testing.T) {
	info := Discover()
	var acct string
	for _, l := range info.Locations {
		if l.Kind == KindBackup && l.Account != "" {
			acct = l.Account
		}
	}
	if acct == "" {
		t.Skip("no computer folder configured in Drive")
	}
	m, err := MirrorInodes(acct)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("mirror items: %d", len(m))
	var ids []int64
	for _, id := range m {
		ids = append(ids, id)
		break
	}
	del, err := RemoteDeleted(acct, ids)
	t.Logf("first item remote-deleted=%v err=%v", del, err)
	if _, err := RemoteDeleted(acct, []int64{-12345}); !errors.Is(err, ErrUnknownItem) {
		t.Errorf("unknown id: %v", err)
	}
}
