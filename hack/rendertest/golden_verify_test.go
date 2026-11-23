package rendertest

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGoldenVerifyAllProfiles invokes hack/verify-golden.sh — the pinned
// kustomize re-render of the upstream raw manifests for ALL THREE profiles
// (NFS working tree; FC and iSCSI checkout-external from the clean driver
// commits recorded in the golden headers), the approved-transforms field
// comparison against the committed goldens, and nothing else. It is the
// Regeneration-independence proof for the FC/iSCSI goldens
// (08 regeneration independence / normalized comparison): the raw source is NEVER derived from the golden, and
// the comparison does not share a code path with the renderer under test.
//
// Like the config/default rendertest, kustomize is resolved from the local
// module cache by the script (sigs.k8s.io/kustomize/kustomize/v5@v5.7.1 —
// the pinned version); a fresh environment downloads it once, and
// afterwards GOPROXY=off cache hits suffice (docs/BUILDING.md "rendertest
// offline durumu"). Missing sibling driver checkouts or missing pins fail
// the script with exit 2 and a message — this test then FAILS (the proof
// is absent), it never skips.
func TestGoldenVerifyAllProfiles(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "hack/verify-golden.sh")
	cmd.Dir = repoRoot
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("hack/verify-golden.sh failed: %v\noutput:\n%s", err, out.String())
	}

	// Six field comparisons must be OK — the golden-verify OK line carries
	// the profile/platform pair, so a missing comparison (e.g. a new
	// profile golden never wired) fails this test.
	text := out.String()
	for _, combo := range []string{
		"golden-verify OK (nfs/k8s)", "golden-verify OK (nfs/ocp)",
		"golden-verify OK (fc/k8s)", "golden-verify OK (fc/ocp)",
		"golden-verify OK (iscsi/k8s)", "golden-verify OK (iscsi/ocp)",
	} {
		if !strings.Contains(text, combo) {
			t.Errorf("verify output lacks %q — full output:\n%s", combo, text)
		}
	}
	if !strings.Contains(text, "verify-golden: OK (nfs+fc+iscsi, both platforms)") {
		t.Errorf("verify output lacks the final all-profiles OK line — full output:\n%s", text)
	}
}
