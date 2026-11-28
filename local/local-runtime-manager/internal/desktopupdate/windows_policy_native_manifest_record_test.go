//go:build !lazymind_unsigned_test

package desktopupdate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newWindowsPolicyNativeManifestRecordFixture(t *testing.T) *nativeStateFixture {
	t.Helper()
	f := newNativeStateFixture(t)
	f.record["mode"] = "windows-policy"
	f.record["platform"], f.record["arch"] = "windows", "amd64"
	f.record["signingIdentity"] = "FIXTURE_WINDOWS_RELEASE"
	f.record["installedManifestSHA256"] = strings.Repeat("e", 64)
	f.record["installedManifestSize"] = MaxEnvelopeBytes
	// The state store records accepted facts. Prepare and the launch guard own
	// artifact verification, so this fixture deliberately supplies no file.
	f.record["installedManifestPath"] = filepath.Join(f.root, "downloads", "installed-manifest.json")
	return f
}

func TestWindowsPolicyNativeManifestRecordRetainsBinding(t *testing.T) {
	f := newWindowsPolicyNativeManifestRecordFixture(t)
	f.create(t)
	assertWindowsPolicyNativeManifestBinding(t, f, "pending")
	f.command(t, "installing", map[string]any{
		"transactionId": f.tx, "dataProfile": f.record["sourceDataProfile"],
		"componentMode": "absent", "sourcePaused": true, "sourceActive": 0,
	}, "installing", "")
	assertWindowsPolicyNativeManifestBinding(t, f, "installing")
}

func assertWindowsPolicyNativeManifestBinding(t *testing.T, f *nativeStateFixture, phase string) {
	t.Helper()
	record, err := ReadNativeRecord(f.root)
	if err != nil || record == nil || record.TransactionID != f.tx || record.Phase != phase {
		t.Fatalf("read %s record: %+v, err=%v", phase, record, err)
	}
	var actual map[string]json.RawMessage
	if err := json.Unmarshal(nativeJSON(t, record), &actual); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"mode", "installedManifestPath", "installedManifestSHA256", "installedManifestSize"} {
		want := nativeJSON(t, f.record[key])
		if !bytes.Equal(actual[key], want) {
			t.Errorf("%s stored %s = %s, want %s", phase, key, actual[key], want)
		}
	}
}

func TestWindowsPolicyNativeManifestRecordRejectsInvalidBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"all manifest fields omitted", func(r map[string]any) {
			delete(r, "installedManifestPath")
			delete(r, "installedManifestSHA256")
			delete(r, "installedManifestSize")
		}},
		{"manifest without mode", func(r map[string]any) { delete(r, "mode") }},
		{"only manifest size without mode", func(r map[string]any) {
			delete(r, "mode")
			delete(r, "installedManifestPath")
			delete(r, "installedManifestSHA256")
		}},
		{"unrecognized mode", func(r map[string]any) { r["mode"] = "windows-policy-unknown" }},
		{"path omitted", func(r map[string]any) { delete(r, "installedManifestPath") }},
		{"relative path", func(r map[string]any) { r["installedManifestPath"] = "installed-manifest.json" }},
		{"digest omitted", func(r map[string]any) { delete(r, "installedManifestSHA256") }},
		{"invalid digest", func(r map[string]any) { r["installedManifestSHA256"] = strings.Repeat("g", 64) }},
		{"size omitted", func(r map[string]any) { delete(r, "installedManifestSize") }},
		{"zero size", func(r map[string]any) { r["installedManifestSize"] = 0 }},
		{"negative size", func(r map[string]any) { r["installedManifestSize"] = -1 }},
		{"size exceeds signed envelope limit", func(r map[string]any) { r["installedManifestSize"] = MaxEnvelopeBytes + 1 }},
		{"macOS platform", func(r map[string]any) { r["platform"] = "darwin" }},
		{"Windows arm64", func(r map[string]any) { r["arch"] = "arm64" }},
		{"test audience", func(r map[string]any) { r["audience"] = "fork-test" }},
		{"canary channel", func(r map[string]any) { r["channel"] = "canary" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWindowsPolicyNativeManifestRecordFixture(t)
			tc.change(f.record)
			f.command(t, "create", f.record, "blocked", "update_request_invalid")
			if _, err := os.Lstat(f.filename); !os.IsNotExist(err) {
				t.Fatalf("invalid manifest binding produced a live transaction: %v", err)
			}
		})
	}
}
