package knowledge_market

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

const (
	prebuiltLFSOrigin = "https://modelscope.cn/datasets/sunjh2222/lazymind-prebuilt-chunks-demo/resolve/master/inline-v1"
	prebuiltLFSCDN    = "https://cdn-lfs-cn-1.modelscope.cn"
	prebuiltLFSQuery  = "?X-Amz-Signature=fixture-lfs-signature&X-Amz-Security-Token=fixture-lfs-session"
	prebuiltLFSItemID = "local-prebuilt-demo"
)

type prebuiltLFSStep struct {
	url      string
	body     []byte
	location string
	err      error
}

func expectPrebuiltLFSRequests(t *testing.T, steps ...prebuiltLFSStep) {
	t.Helper()
	previous := http.DefaultTransport
	requestCount := 0
	t.Cleanup(func() {
		http.DefaultTransport = previous
		if requestCount != len(steps) {
			t.Errorf("download made %d requests, want %d", requestCount, len(steps))
		}
	})
	http.DefaultTransport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if requestCount >= len(steps) {
			t.Fatalf("download sent an unexpected request to %s%s", request.URL.Host, request.URL.Path)
		}
		step := steps[requestCount]
		requestCount++
		if request.Method != http.MethodGet || request.URL.String() != step.url {
			t.Fatalf("request %d has an unexpected method, path or signed query: %s %s%s",
				requestCount, request.Method, request.URL.Host, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
			t.Fatal("package download must be anonymous")
		}
		if step.err != nil {
			return nil, step.err
		}
		response := &http.Response{
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			Body:          io.NopCloser(bytes.NewReader(step.body)),
			ContentLength: int64(len(step.body)),
		}
		if step.location != "" {
			response.StatusCode = http.StatusFound
			response.Header.Set("Location", step.location)
		}
		return response, nil
	})
}

func assertPrebuiltLFSErrorRedacted(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the package download to fail")
	}
	for _, secret := range []string{"fixture-lfs-signature", "fixture-lfs-session", "X-Amz-Signature", "X-Amz-Security-Token"} {
		if strings.Contains(err.Error(), secret) {
			t.Error("the returned error includes a signed CDN query")
			break
		}
	}
}

func TestPrebuiltModelScopeLFSDownloadsAndValidatesPinnedPackage(t *testing.T) {
	demo, _ := prebuiltDemoFiles(t)
	const manifestPath = "/releases/v1/manifest.json"
	var manifest prebuiltManifest
	if err := json.Unmarshal(demo[manifestPath], &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Documents) != 2 {
		t.Fatal("the frozen release must contain two documents")
	}
	for _, tc := range []struct {
		name     string
		tampered string
		want     string
	}{
		{name: "download and decode both documents"},
		{name: "tampered manifest from allowed CDN", tampered: manifestPath, want: "manifest checksum"},
		{name: "tampered object from allowed CDN", tampered: "/" + manifest.Documents[0].ObjectPath, want: "size or checksum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pointerURL := prebuiltLFSOrigin + "/latest.json"
			steps := []prebuiltLFSStep{{url: pointerURL, body: demo["/latest.json"]}}
			for _, path := range []string{manifestPath, "/" + manifest.Documents[0].ObjectPath, "/" + manifest.Documents[1].ObjectPath} {
				location := prebuiltLFSCDN + "/lfs-fixture" + path + prebuiltLFSQuery
				data := demo[path]
				if path == tc.tampered {
					data = bytes.Clone(data)
					if path == manifestPath {
						data = append(data, '\n') // The JSON remains valid; the pinned digest changes.
					} else {
						data[4] ^= 1 // Change only the gzip timestamp, preserving size and decoded text.
					}
				}
				steps = append(steps, prebuiltLFSStep{url: prebuiltLFSOrigin + path, location: location},
					prebuiltLFSStep{url: location, body: data})
				if path == tc.tampered {
					break
				}
			}
			expectPrebuiltLFSRequests(t, steps...)
			pinned, err := readPrebuiltRelease(ctx, pointerURL, prebuiltLFSItemID)
			if err != nil {
				t.Fatalf("read the same-origin pointer: %v", err)
			}
			if pinned.URL != pointerURL || pinned.Version != "v1" || pinned.DocumentCount != 2 {
				t.Fatal("the same-origin pointer did not pin the frozen release")
			}
			root := t.TempDir()
			files, err := fetchPrebuiltFiles(ctx, pinned, root)
			if tc.tampered != "" {
				assertPrebuiltLFSErrorRedacted(t, err)
				if !strings.Contains(err.Error(), tc.want) || len(files) != 0 {
					t.Error("the existing checksum validator must reject tampered CDN bytes without returning any files")
				}
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 0 {
					t.Fatal("a rejected download left staged file data")
				}
				return
			}
			if err != nil {
				t.Fatal("the approved CDN could not deliver the SHA-pinned package")
			}
			if len(files) != len(manifest.Documents) {
				t.Fatalf("decoded %d documents, want %d", len(files), len(manifest.Documents))
			}
			for i, document := range manifest.Documents {
				file := files[i]
				original, textErr := os.ReadFile(file.LocalPath)
				compressed, objectErr := os.ReadFile(file.PrebuiltPath)
				wantText := map[string]string{"reading-room": "14:30", "tool-shelf": "16:40"}[document.ID]
				if wantText == "" || textErr != nil || objectErr != nil || !bytes.Contains(original, []byte(wantText)) ||
					!bytes.Equal(compressed, demo["/"+document.ObjectPath]) || file.SourcePath != document.SourcePath ||
					file.PrebuiltIdentity != pinned.ManifestSHA256+":"+document.ID {
					t.Errorf("downloaded text, verified object, or pinned provenance does not match %s", document.ID)
				}
			}
		})
	}
}

func TestPrebuiltModelScopeLFSPreservesSameOriginPointerRedirect(t *testing.T) {
	demo, _ := prebuiltDemoFiles(t)
	pointerURL := prebuiltLFSOrigin + "/latest.json"
	stableURL := prebuiltLFSOrigin + "/latest-stable.json"
	expectPrebuiltLFSRequests(t,
		prebuiltLFSStep{url: pointerURL, location: stableURL},
		prebuiltLFSStep{url: stableURL, body: demo["/latest.json"]},
	)
	pinned, err := readPrebuiltRelease(context.Background(), pointerURL, prebuiltLFSItemID)
	if err != nil || pinned == nil || pinned.Version != "v1" || pinned.URL != pointerURL {
		t.Fatal("the existing same-origin redirect must still pin v1 at the configured source")
	}
}

func TestPrebuiltModelScopeLFSRedirectPolicy(t *testing.T) {
	demo, _ := prebuiltDemoFiles(t)
	const signedPath = "/lfs-fixture/data" + prebuiltLFSQuery
	for _, tc := range []struct {
		name                string
		origin              string
		target              string
		fromPointer         bool
		firstTargetResponse *prebuiltLFSStep
	}{
		{name: "version pointer cannot follow even the approved CDN", target: prebuiltLFSCDN, fromPointer: true},
		{name: "other ModelScope CDN", target: "https://cdn-lfs-cn-2.modelscope.cn"},
		{name: "arbitrary origin to approved CDN", origin: "https://mirror.example.test/datasets/acme/demo/resolve/master", target: prebuiltLFSCDN},
		{name: "lookalike ModelScope origin", origin: "https://modelscope.cn.example.test/datasets/acme/demo/resolve/master", target: prebuiltLFSCDN},
		{name: "ModelScope path outside dataset resolve", origin: "https://modelscope.cn/other/datasets/acme/demo/resolve/master", target: prebuiltLFSCDN},
		{name: "HTTPS downgrade to approved host", target: "http://cdn-lfs-cn-1.modelscope.cn"},
		{name: "lookalike CDN host", target: "https://cdn-lfs-cn-1.modelscope.cn.example.test"},
		{name: "unexpected port on approved host", target: prebuiltLFSCDN + ":8443"},
		{name: "userinfo on approved host", target: "https://fixture@cdn-lfs-cn-1.modelscope.cn"},
		{name: "malformed signed location", target: prebuiltLFSCDN + "/bad%zz"},
		{name: "second redirect even on approved host", target: prebuiltLFSCDN, firstTargetResponse: &prebuiltLFSStep{
			location: prebuiltLFSCDN + "/another-lfs-file" + prebuiltLFSQuery,
		}},
		{name: "second redirect back from CDN to ModelScope", target: prebuiltLFSCDN, firstTargetResponse: &prebuiltLFSStep{
			location: prebuiltLFSOrigin + "/another-lfs-file" + prebuiltLFSQuery,
		}},
		{name: "CDN after an initial same-origin redirect", target: prebuiltLFSOrigin, firstTargetResponse: &prebuiltLFSStep{
			location: prebuiltLFSCDN + "/another-lfs-file" + prebuiltLFSQuery,
		}},
		{name: "transport failure at signed target", target: prebuiltLFSCDN, firstTargetResponse: &prebuiltLFSStep{
			err: errors.New("simulated CDN connection reset"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := tc.origin
			if origin == "" {
				origin = prebuiltLFSOrigin
			}
			pointerURL := origin + "/latest.json"
			location := tc.target + signedPath
			pointer := prebuiltLFSStep{url: pointerURL, body: demo["/latest.json"]}
			steps := []prebuiltLFSStep{pointer}
			if tc.fromPointer {
				steps[0].location = location
			} else {
				steps = append(steps, prebuiltLFSStep{url: origin + "/releases/v1/manifest.json", location: location})
			}
			if tc.firstTargetResponse != nil {
				response := *tc.firstTargetResponse
				response.url = location
				steps = append(steps, response)
			}
			expectPrebuiltLFSRequests(t, steps...)
			ctx := context.Background()
			pinned, err := readPrebuiltRelease(ctx, pointerURL, prebuiltLFSItemID)
			if tc.fromPointer {
				assertPrebuiltLFSErrorRedacted(t, err)
				if pinned != nil {
					t.Fatal("a redirected version pointer was accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("read the same-origin pointer: %v", err)
			}
			files, err := fetchPrebuiltFiles(ctx, pinned, t.TempDir())
			assertPrebuiltLFSErrorRedacted(t, err)
			if len(files) != 0 {
				t.Fatal("a rejected download returned installable files")
			}
		})
	}
}
