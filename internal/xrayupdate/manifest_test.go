package xrayupdate

import "testing"

func TestDecodeManifestRequiresReleaseAndRuntimeVersions(t *testing.T) {
	raw := []byte(`{
  "schemaVersion": 2,
  "version": "v26.7.28-xui.1",
  "xrayVersion": "26.7.28",
  "publishedAt": "2026-07-30T12:00:00Z",
  "artifacts": [{
    "os": "linux",
    "arch": "amd64",
    "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "size": 1
  }]
}`)
	manifest, err := decodeManifest(raw)
	if err != nil {
		t.Fatalf("decodeManifest: %v", err)
	}
	if manifest.Version != "v26.7.28-xui.1" || manifest.XrayVersion != "26.7.28" {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	withoutRuntime := []byte(`{"schemaVersion":2,"version":"v26.7.28-xui.1","publishedAt":"2026-07-30T12:00:00Z","artifacts":[{"os":"linux","arch":"amd64","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":1}]}`)
	if _, err := decodeManifest(withoutRuntime); err == nil {
		t.Fatal("manifest without xrayVersion was accepted")
	}
}
