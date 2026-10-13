package jobs

// The fake engine (FK-04). lib/jobs/testdata/r10k-5.0.3/deploy-success.json is
// a verbatim copy of stagehand-sdk-public
// harness/fixtures/r10k-5.0.3/deploy-success.json (a real r10k 5.0.3 run
// recorded by the SDK harness). Do not edit it; re-copy it from the SDK.

import (
	"os"
	"testing"

	"github.com/voxpupuli/webhook-go/lib/jobs/fakeengine"
)

func TestMain(m *testing.M) {
	fakeengine.MaybeRun() // becomes the fake r10k when the fixture env var is set
	os.Exit(m.Run())
}
