package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// Marker is the deploy marker an engine writes into an environment:
// <basedir>/<env>/.r10k-deploy.json (deploy_success, finished_at,
// module_deploys, name, signature, started_at) or .g10k-deploy.json
// (deploy_success, finished_at, git_dir, git_url, name, puppetfile_checksum,
// signature, started_at). Only the keys the verdict needs are decoded.
type Marker struct {
	Name          string `json:"name"`
	Signature     string `json:"signature"`
	DeploySuccess *bool  `json:"deploy_success"`
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at"`
}

// r10kTimeLayout is how r10k writes marker times (Ruby Time#to_s).
const r10kTimeLayout = "2006-01-02 15:04:05 -0700"

var markerTimeLayouts = []string{
	time.RFC3339Nano, // g10k (Go time.Time)
	r10kTimeLayout,
	"2006-01-02 15:04:05.999999999 -0700",
	"2006-01-02 15:04:05 MST",
}

func parseMarkerTime(s string) (time.Time, error) {
	for _, layout := range markerTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q", s)
}

// ReadMarker reads a marker file. A missing file is (nil, nil): the verdict
// then says "no deploy marker". An unreadable or corrupt file is an error.
func ReadMarker(path string) (*Marker, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &m, nil
}

// Normalize decides whether one environment was deployed, the same way for
// every engine (D-29). It is deployed only when the engine exited 0, the
// marker exists, the marker was rewritten by this run (finished_at at or after
// jobStart), the marker says deploy_success true, and its signature equals
// expected. Anything else is failed, with errText naming the first unmet
// condition. An exit code alone proves nothing: g10k exits 0 for an unknown
// environment and an unreachable remote, and r10k can move a checkout under an
// unchanged marker.
//
// deployedSHA is the marker's signature when the marker was written by this
// run, and empty otherwise, so a stale marker is never reported as what this
// run deployed.
func Normalize(exit int, marker *Marker, jobStart time.Time, expected string) (state State, deployedSHA, errText string) {
	fresh := false
	var freshErr string
	if marker != nil {
		finished, err := parseMarkerTime(marker.FinishedAt)
		switch {
		case err != nil:
			freshErr = "marker finished_at unreadable: " + marker.FinishedAt
		// r10k writes whole seconds, so compare against the second the run started.
		case finished.Before(jobStart.Truncate(time.Second)):
			freshErr = "marker not rewritten by this run"
		default:
			fresh = true
			deployedSHA = marker.Signature
		}
	}

	switch {
	case exit != 0:
		return StateFailed, deployedSHA, fmt.Sprintf("engine exited %d", exit)
	case marker == nil:
		return StateFailed, "", "no deploy marker"
	case !fresh:
		return StateFailed, "", freshErr
	case marker.DeploySuccess == nil || !*marker.DeploySuccess:
		return StateFailed, deployedSHA, "marker reports deploy_success false"
	case marker.Signature != expected:
		return StateFailed, deployedSHA, fmt.Sprintf("deployed %s, expected %s", marker.Signature, expected)
	}
	return StateDeployed, deployedSHA, ""
}
