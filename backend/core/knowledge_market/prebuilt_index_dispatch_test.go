package knowledge_market

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"lazymind/core/asyncjob"
	"lazymind/core/common/orm"
)

func TestClaimPrebuiltIndexDispatchChecksCurrentReleaseAndAllowsOneSend(t *testing.T) {
	for _, publishedVersion := range []string{"v1", "v2"} {
		t.Run(publishedVersion, func(t *testing.T) {
			files, _ := prebuiltDemoFiles(t)
			var confirmed prebuiltRelease
			if err := json.Unmarshal(files["/latest.json"], &confirmed); err != nil {
				t.Fatal(err)
			}
			confirmed.URL = previewOrigin + "/latest.json"
			manifestPath := "/" + confirmed.ManifestPath
			published, manifest := confirmed, json.RawMessage(files[manifestPath])
			if publishedVersion != confirmed.Version {
				var decoded prebuiltManifest
				if err := json.Unmarshal(manifest, &decoded); err != nil {
					t.Fatal(err)
				}
				decoded.Version = publishedVersion
				manifest = previewJSON(t, decoded)
				published.Version = publishedVersion
				published.ManifestPath = "releases/" + publishedVersion + "/manifest.json"
				published.ManifestSHA256 = previewHash(string(manifest))
			}

			db, _ := previewMarket(t, confirmed)
			installed, err := loadMarketInstallRow(t.Context(), db, previewUserID, previewItemID)
			if err != nil || installed == nil {
				t.Fatalf("load installed release: %v", err)
			}
			job := asyncjob.Job{ID: "index-dispatch", JobType: installJobType, AttemptCount: 1}
			lease := time.Now().Add(time.Minute)
			if err := db.Create(&orm.AsyncJob{ID: job.ID, JobType: job.JobType, ResourceID: previewItemID, CreateUserID: previewUserID,
				Status: string(asyncjob.StatusRunning), AttemptCount: job.AttemptCount, LockUntil: &lease}).Error; err != nil {
				t.Fatal(err)
			}
			paths := interceptPreviewRequests(t, map[string]json.RawMessage{
				"/latest.json": previewJSON(t, published), "/" + published.ManifestPath: manifest,
			})
			payload := installJobPayload{MarketItemID: previewItemID, UserID: previewUserID, Prebuilt: &confirmed}
			taskIDs := decodeInstallConfig(installed).TaskIDs
			claim := func() error {
				return claimPrebuiltIndexDispatch(t.Context(), db, job, payload, installed.DatasetID, taskIDs)
			}
			wantClaim := ""
			if publishedVersion == confirmed.Version {
				if err := claim(); err != nil {
					t.Fatalf("claim unchanged release: %v", err)
				}
				wantClaim = job.ID
				if err := claim(); !errors.Is(err, errPrebuiltIndexAlreadyStarted) {
					t.Fatalf("repeat claim = %v, want refusal", err)
				}
				paths("/latest.json", manifestPath, "/latest.json", manifestPath)
			} else {
				if err := claim(); !errors.Is(err, errStaleInstallConfirmation) {
					t.Fatalf("claim after release update = %v, want stale confirmation", err)
				}
				paths("/latest.json", "/"+published.ManifestPath)
			}
			current, err := loadMarketInstallRow(t.Context(), db, previewUserID, previewItemID)
			if err != nil || current == nil || current.DatasetID != installed.DatasetID || current.InstalledVersion != confirmed.Version ||
				current.InstallState != string(orm.InstallStateDone) || decodeInstallConfig(current).IndexDispatchJobID != wantClaim {
				t.Fatalf("claim changed the installed text or stored the wrong dispatch marker: %v", err)
			}
		})
	}
}
