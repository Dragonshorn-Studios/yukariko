package store

import (
	"context"
	"testing"
	"time"
)

// Deployment rows must carry real versions: from_version is the pre-deploy
// checkpoint recorded at claim, to_version the committed value written in
// the success transaction. Webhook payloads and the deployments API read
// them; empty columns would mean "we deployed nothing, to nowhere".
func TestDeploymentRowCarriesVersions(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	depID, err := s.BeginDeployment(ctx, BeginDeploymentParams{
		AppID: "web", Cause: "update", FromVersion: "sha256:old", At: now,
	})
	if err != nil {
		t.Fatalf("BeginDeployment: %v", err)
	}
	if err := s.CommitDeploymentSuccess(ctx, "web", KindDigest, "sha256:new", depID, now.Add(time.Minute)); err != nil {
		t.Fatalf("CommitDeploymentSuccess: %v", err)
	}
	rows, err := s.RecentDeployments(ctx, "web", 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("RecentDeployments = %v, %v", rows, err)
	}
	if rows[0].FromVersion != "sha256:old" || rows[0].ToVersion != "sha256:new" {
		t.Errorf("row versions = %q -> %q; want sha256:old -> sha256:new", rows[0].FromVersion, rows[0].ToVersion)
	}
	if rows[0].Status != StatusSucceeded {
		t.Errorf("status = %q, want succeeded", rows[0].Status)
	}
}
