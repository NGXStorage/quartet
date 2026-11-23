package v1alpha1

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var testDigest = "quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:" + strings.Repeat("a", 64)

func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		crName  string
		wantErr bool
	}{
		{"singleton accepted", "quartet", false},
		{"foreign name rejected", "quartet-extra", true},
		{"empty name rejected", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &Quartet{ObjectMeta: metav1.ObjectMeta{Name: tc.crName}}
			if err := q.ValidateName(); (err != nil) != tc.wantErr {
				t.Fatalf("ValidateName() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateSpec(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(q *Quartet)
		wantErr bool
		errSub  string
	}{
		{"fully valid", func(q *Quartet) {}, false, ""},
		{"disabled profiles may omit refs", func(q *Quartet) {
			q.Spec.Profiles.NFS.Enabled = false
			q.Spec.Profiles.NFS.SecretRef = nil
			q.Spec.Profiles.NFS.Image = nil
		}, false, ""},
		{"bad platform enum", func(q *Quartet) {
			q.Spec.Platform = Platform("windows")
		}, true, "platform"},
		{"bad logLevel enum", func(q *Quartet) {
			q.Spec.LogLevel = LogLevel("verbose")
		}, true, "logLevel"},
		{"enabled without secretRef", func(q *Quartet) {
			q.Spec.Profiles.NFS.SecretRef = nil
		}, true, "secretRef"},
		{"enabled without image", func(q *Quartet) {
			q.Spec.Profiles.NFS.Image = nil
		}, true, "image"},
		{"image tag without digest rejected", func(q *Quartet) {
			q.Spec.Profiles.NFS.Image.Reference = "quay.io/ngxstorage/nfs:v1.2.3"
		}, true, "sha256"},
		{"non-hex digest rejected", func(q *Quartet) {
			q.Spec.Profiles.NFS.Image.Reference = "quay.io/ngxstorage/nfs@sha256:" + strings.Repeat("g", 64)
		}, true, "sha256"},
		{"secretRef with uppercase rejected", func(q *Quartet) {
			q.Spec.Profiles.NFS.SecretRef.Name = "NFS-Config"
		}, true, "secretRef"},
		{"secretRef with slash rejected", func(q *Quartet) {
			q.Spec.Profiles.NFS.SecretRef.Name = "nfs/config"
		}, true, "secretRef"},
		{"fc profile also validated", func(q *Quartet) {
			q.Spec.Profiles.FC.Enabled = true
		}, true, "fc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := validQuartet()
			tc.mutate(q)
			err := q.ValidateSpec()
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateSpec() err=%v, wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("ValidateSpec() err=%q, want substring %q", err, tc.errSub)
			}
		})
	}
}

func TestValidateImmutablePlatform(t *testing.T) {
	q := validQuartet()
	if err := q.ValidateImmutablePlatform(); err != nil {
		t.Fatalf("first acceptance must pass, got %v", err)
	}
	q.Status.SpecPlatform = q.Spec.Platform
	if err := q.ValidateImmutablePlatform(); err != nil {
		t.Fatalf("unchanged platform must pass, got %v", err)
	}
	q.Spec.Platform = PlatformOpenShift
	if err := q.ValidateImmutablePlatform(); err == nil {
		t.Fatal("platform change after acceptance must be rejected")
	}
}

// TestSecretNameBoundary pins the REAL Kubernetes Secret metadata.name
// contract: RFC 1123 DNS subdomain — dots are legal, the limit is 253
// chars. The old DNS-label pattern wrongly rejected legal names; the API
// server's own IsDNS1123Subdomain validation is mirrored, not exceeded.
func TestSecretNameBoundary(t *testing.T) {
	accept := []string{
		"nfs-csi-ngxstorage-config", // canonical driver config name
		"my.secret.name",            // dots are legal in a DNS subdomain
		"a.b",
		"sub.domain.example",
		"x1.y2",
		strings.Repeat("a.", 126) + "b", // exactly 253 chars
	}
	reject := []string{
		"NFS-Config",                     // uppercase
		"nfs/config",                     // slash
		"nfs_x",                          // underscore (not in metadata.name)
		"-nfs",                           // leading separator
		"nfs-",                           // trailing separator
		".nfs",                           // leading dot
		"nfs.",                           // trailing dot
		"",                               // empty
		strings.Repeat("a.", 126) + "bc", // 254 chars
	}
	for _, tc := range accept {
		q := validQuartet()
		q.Spec.Profiles.NFS.SecretRef.Name = tc
		if err := q.ValidateSpec(); err != nil {
			t.Errorf("secret name %q must be accepted (legal RFC 1123 subdomain), got %v", tc, err)
		}
	}
	for _, tc := range reject {
		q := validQuartet()
		q.Spec.Profiles.NFS.SecretRef.Name = tc
		if err := q.ValidateSpec(); err == nil {
			t.Errorf("secret name %q must be rejected, got nil", tc)
		}
	}
}

// TestImageReferenceBoundary pins the OCI digest-reference contract at the
// reconciler-side re-check: registry and path mandatory, port
// allowed, sha256 digest mandatory — a bare tag or a digest-less reference
// is never accepted (no wrong-acceptance), and the shape mirrors the CRD
// pattern.
func TestImageReferenceBoundary(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	accept := []string{
		"quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:" + hex64, // canonical digest-pinned reference
		"localhost:5000/repo@sha256:" + hex64,                   // registry with port
		"192.168.1.10:5000/repo@sha256:" + hex64,                // IP literal registry
		"REGISTRY.example.com/repo@sha256:" + hex64,             // hostnames are case-insensitive
		"registry.example.com/a/b_c.d-e@sha256:" + hex64,        // OCI path component grammar
	}
	reject := []string{
		"quay.io/ngxstorage/nfs:v1.2.3",                   // tag without digest
		"quay.io/repo",                                    // no digest at all
		"quay.io/repo@",                                   // empty digest
		"quay.io/repo@sha256:abc",                         // digest too short
		"quay.io/repo@sha256:" + strings.Repeat("g", 64),  // non-hex digest
		"quay.io/repo@sha512:" + strings.Repeat("a", 128), // wrong algorithm (sha256 only)
		"quay.io@sha256:" + hex64,                         // missing repository path
		"quay.io/Repo@sha256:" + hex64,                    // uppercase path (OCI normalizes to lowercase; stricter here)
		"registry.example.com./repo@sha256:" + hex64,      // trailing-dot hostname
		"_registry.example.com/repo@sha256:" + hex64,      // underscore not a legal hostname start
		"", // empty
	}
	for _, tc := range accept {
		q := validQuartet()
		q.Spec.Profiles.NFS.Image.Reference = tc
		if err := q.ValidateSpec(); err != nil {
			t.Errorf("image reference %q must be accepted (OCI digest reference), got %v", tc, err)
		}
	}
	for _, tc := range reject {
		q := validQuartet()
		q.Spec.Profiles.NFS.Image.Reference = tc
		if err := q.ValidateSpec(); err == nil {
			t.Errorf("image reference %q must be rejected, got nil", tc)
		}
	}
}

// TestFCISCSIProfileSupportBoundary pins the support contract:
// disabled FC/iSCSI profiles are valid specs (no operand, no claim); an
// ENABLED FC/iSCSI profile is NOT rejected by the API-layer validation —
// renderer availability is a controller-side concern (surfaced per profile
// in status, asserted by TestEnabledFCProfileRendersIsolated and
// TestEnvtestEnabledFCProfileIsolated), never an InvalidSpec at the API
// layer, and never silently disabled or Ready.
func TestFCISCSIProfileSupportBoundary(t *testing.T) {
	q := validQuartet()
	q.Spec.Profiles.FC = ProfileSpec{Enabled: false}
	q.Spec.Profiles.ISCSI = ProfileSpec{Enabled: false, SecretRef: nil, Image: nil}
	if err := q.ValidateSpec(); err != nil {
		t.Fatalf("disabled FC/iSCSI must stay a valid spec, got %v", err)
	}

	q2 := validQuartet()
	q2.Spec.Profiles.FC = ProfileSpec{
		Enabled:   true,
		SecretRef: &SecretRef{Name: "fc-csi-ngxstorage-config"},
		Image:     &ImageSpec{Reference: "quay.io/ngxstorage/fc-csi-ngxstorage@sha256:" + strings.Repeat("d", 64)},
	}
	if err := q2.ValidateSpec(); err != nil {
		t.Fatalf("enabled FC with valid refs must pass API-layer validation (renderer availability is a controller-side status surface, not InvalidSpec), got %v", err)
	}
}

// TestValidReasonsSet pins the reason set: exactly these 14 values, none
// containing a slash (metav1.Condition constraint). The draft reason
// DataInUse is deliberately NOT here: ValidReasons() pins the frozen
// reason set, and DataInUse is an addition to the v1alpha1 draft
// surface pending API finalization (not a published contract).
func TestValidReasonsSet(t *testing.T) {
	want := map[string]bool{
		"Ready": true, "MissingSecret": true, "MissingSecretKey": true,
		"InvalidSpec": true, "UnsupportedPlatform": true, "PlatformDetectionFailed": true,
		"NamespaceMissing": true, "PermissionDenied": true, "ForeignResourceConflict": true,
		"DependencyMissing": true, "RenderError": true, "ApplyConflict": true,
		"WorkloadNotReady": true, "CleanupFailed": true,
	}
	got := ValidReasons()
	if len(got) != len(want) {
		t.Fatalf("ValidReasons() has %d entries, want %d", len(got), len(want))
	}
	for r := range want {
		if !got[r] {
			t.Fatalf("ValidReasons() missing %q", r)
		}
		if strings.ContainsRune(r, '/') {
			t.Fatalf("reason %q must not contain a slash", r)
		}
	}
}

// validQuartet builds a spec-valid singleton: nfs enabled with canonical
// secret name and a digest-pinned reference.
func validQuartet() *Quartet {
	return &Quartet{
		ObjectMeta: metav1.ObjectMeta{Name: QuartetName},
		Spec: QuartetSpec{
			Platform: PlatformAuto,
			LogLevel: LogLevelInfo,
			Profiles: Profiles{
				NFS: ProfileSpec{
					Enabled:   true,
					SecretRef: &SecretRef{Name: "nfs-csi-ngxstorage-config"},
					Image:     &ImageSpec{Reference: testDigest},
				},
			},
		},
	}
}
