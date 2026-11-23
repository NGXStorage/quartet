package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/renderer"
)

var (
	testEnv     *envtest.Environment
	testCfg     *rest.Config
	testScheme  = runtime.NewScheme()
	testManager ctrl.Manager
	testClient  client.Client
	// testReconciler is the envtest-plane reconciler instance — exposed so
	// tests can issue a DIRECT reconcile call (no watch event), which the
	// bounded-retry convergence test needs (an edit's own
	// event may be consumed in-flight, so recovery cannot rely on "a later
	// event will come").
	testReconciler *QuartetReconciler
	mgrCancel      context.CancelFunc
	mgrShutdown    func()

	envtestOnce    sync.Once
	envtestFailed  error
	envtestStarted bool
	envtestSkipsMu sync.Mutex
	envtestSkips   int
)

const suiteTimeout = 30 * time.Second

// TestMain runs the package tests; envtest itself is lazily started for the
// TestEnvtest* tests only, so plain unit tests run without assets.
//
// The SKIP path is no longer silent. QUARTET_TEST_ENVTEST
// selects required|optional (default: auto = required when KUBEBUILDER_ASSETS
// is set, optional otherwise). In REQUIRED mode a missing asset set or a
// startup failure FAILS the suite, and the machine-verified skip count must
// be zero at TestMain exit — an observed failure mode (assets
// present, API server refused to listen, every TestEnvtest* skipped, exit 0)
// is now impossible.
func TestMain(m *testing.M) {
	mode := envtestRequiredMode()
	if mode && os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "FAIL: QUARTET_TEST_ENVTEST=required but KUBEBUILDER_ASSETS is empty (run `make test-envtest`)")
		os.Exit(1)
	}
	code := m.Run()
	stopEnvtest()
	if mode {
		envtestSkipsMu.Lock()
		skips := envtestSkips
		envtestSkipsMu.Unlock()
		if !envtestStarted {
			fmt.Fprintln(os.Stderr, "FAIL: envtest required mode but the suite never started the API server")
			os.Exit(1)
		}
		if skips != 0 {
			fmt.Fprintf(os.Stderr, "FAIL: envtest required mode completed with %d skip(s) — required mode must have SKIP=0\n", skips)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "envtest gate: required mode, API server started, skips=0 (machine-verified)")
	}
	os.Exit(code)
}

// envtestRequiredMode resolves QUARTET_TEST_ENVTEST: required|optional, with
// auto-defaulting to required when assets are present (the silent-skip
// window is exactly "assets set but startup broken").
func envtestRequiredMode() bool {
	switch strings.ToLower(os.Getenv("QUARTET_TEST_ENVTEST")) {
	case "optional":
		return false
	case "required":
		return true
	default:
		return os.Getenv("KUBEBUILDER_ASSETS") != ""
	}
}

// requireEnvtest starts the real API server (envtest) with the generated CRD
// and the real controller + real discovery client exactly once. In required
// mode a failure is t.Fatalf; in optional mode (no assets) it skips with an
// explicit BLOCKED marker — and the skip is counted for TestMain's
// machine-verified zero-skip assertion.
func requireEnvtest(t *testing.T) {
	t.Helper()
	required := envtestRequiredMode()
	envtestOnce.Do(func() {
		if os.Getenv("KUBEBUILDER_ASSETS") == "" {
			envtestFailed = fmt.Errorf("BLOCKED: envtest assets (KUBEBUILDER_ASSETS empty — run `make test-envtest`)")
			return
		}
		envtestFailed = startEnvtest()
		if envtestFailed == nil {
			envtestStarted = true
		}
	})
	if envtestFailed != nil {
		if required {
			t.Fatalf("envtest required but unavailable: %v", envtestFailed)
		}
		envtestSkipsMu.Lock()
		envtestSkips++
		envtestSkipsMu.Unlock()
		t.Skipf("SKIP: %v", envtestFailed)
	}
}

func startEnvtest() error {
	utilruntime.Must(clientgoscheme.AddToScheme(testScheme))
	utilruntime.Must(v1alpha1.AddToScheme(testScheme))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		Scheme:                testScheme,
	}

	var err error
	testCfg, err = testEnv.Start()
	if err != nil {
		return fmt.Errorf("start envtest: %w", err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(testCfg)
	if err != nil {
		return fmt.Errorf("create discovery client: %w", err)
	}

	testManager, err = ctrl.NewManager(testCfg, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	// Uncached client for the reconciler's safety-critical fresh reads
	// (ownership gate + cleanup enumeration) — mirrors main.go wiring.
	directClient, err := client.New(testCfg, client.Options{Scheme: testScheme})
	if err != nil {
		return fmt.Errorf("create direct client: %w", err)
	}
	testReconciler = &QuartetReconciler{
		Client:            testManager.GetClient(),
		DirectClient:      directClient,
		Scheme:            testManager.GetScheme(),
		Log:               logf.Log.WithName("test").WithName("Quartet"),
		PlatformDiscovery: discoveryClient,
	}
	if err := testReconciler.SetupWithManager(testManager); err != nil {
		return fmt.Errorf("set up controller: %w", err)
	}

	mgrDone := make(chan error, 1)
	mgrCtx, cancel := context.WithCancel(context.Background())
	mgrCancel = cancel
	go func() {
		mgrDone <- testManager.Start(mgrCtx)
	}()
	mgrShutdown = func() {
		// controller-runtime v0.21 Manager has no Stop method: cancel the
		// start context and wait for the Start goroutine to return.
		mgrCancel()
		select {
		case err := <-mgrDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				fmt.Fprintf(os.Stderr, "manager shutdown error: %v\n", err)
			}
		case <-time.After(suiteTimeout):
			fmt.Fprintln(os.Stderr, "manager did not shut down in time")
		}
	}
	if !testManager.GetCache().WaitForCacheSync(mgrCtx) {
		return fmt.Errorf("cache failed to sync")
	}

	if testClient, err = client.New(testCfg, client.Options{Scheme: testScheme}); err != nil {
		return fmt.Errorf("create direct client: %w", err)
	}

	// Out-of-band provisioning: the manager runtime
	// is read-only on RBAC, so the installer-owned operand set — namespace
	// FIRST, then the RBAC set — must exist before any reconcile can pass
	// the RBACDrift health gate. The suite applies the COMMITTED kubernetes
	// install set exactly as an admin installer would — proving the
	// committed files are the provisioning surface and the gate sees a
	// healthy install.
	//
	// The shared plane is provisioned with the NFS install set ONLY: the suite
	// runs a concurrent manager, and provisioning every rendered profile here
	// races the NFS-focused tests' direct Reconcile calls (see
	// applyCommittedInstallSet's note). Tests that enable fc/iscsi provision
	// that profile's set themselves.
	return applyCommittedInstallSet(testClient, v1alpha1.ProfileNFS)
}

// applyCommittedInstallSet parses the committed kubernetes install set of ONE
// profile (config/operands/<profile>/k8s/{namespace.yaml, rbac.yaml} — the
// exact files that profile's k8s entrypoint kustomization lists, in the same
// order) and applies every object with kubectl-apply semantics (create-if-
// absent, OVERWRITE-if-present): the envtest analogue of the admin running
// `kubectl apply -k config/operands/<profile>/k8s`, including the RESTORE use —
// an admin re-applying the set must heal a drifted object, not skip it (the
// recovery use). The namespace is first: the namespaced Role/RoleBinding
// objects target it. A profile without a renderer contributes no committed set
// (nothing to apply).
//
// The REAL envtest plane is deliberately provisioned profile-by-profile by its
// callers (the shared suite setup applies only nfs), NOT every rendered profile
// at once: the suite runs a concurrent manager, and provisioning another
// profile's namespace+RBAC into the shared plane mid-test triggers a manager
// reconcile that races a test's direct Reconcile (409 "object has been
// modified"). A test that enables fc/iscsi provisions that profile's set
// itself.
func applyCommittedInstallSet(c client.Client, profile string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if renderer.For(renderer.ProfileInput{Profile: profile, Platform: v1alpha1.PlatformKubernetes}) == nil {
		return nil // unrendered profile: no committed install set to apply
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		return err
	}
	rel := []string{
		filepath.Join("config", "operands", profile, "k8s", "namespace.yaml"),
		filepath.Join("config", "operands", profile, "k8s", "rbac.yaml"),
	}
	for _, p := range rel {
		raw, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			return fmt.Errorf("read committed install manifest %s: %w", p, err)
		}
		for _, doc := range splitInstallDocs(string(raw)) {
			trimmed := strings.TrimSpace(doc)
			if trimmed == "" {
				continue
			}
			u := &unstructured.Unstructured{}
			if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
				return fmt.Errorf("parse install manifest doc: %w", err)
			}
			if u.GetKind() == "" {
				continue // header/comment-only block
			}
			existing := u.DeepCopy()
			if err := c.Get(ctx, client.ObjectKeyFromObject(u), existing); err == nil {
				// kubectl-apply semantics: overwrite with the committed
				// document (server replaces spec; the stored RV rides the
				// update precondition).
				u.SetResourceVersion(existing.GetResourceVersion())
				if err := c.Update(ctx, u); err != nil {
					return fmt.Errorf("update install object %s/%s: %w", u.GetKind(), u.GetName(), err)
				}
				continue
			} else if !apierrors.IsNotFound(err) {
				return fmt.Errorf("get install object %s/%s: %w", u.GetKind(), u.GetName(), err)
			}
			if err := c.Create(ctx, u); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create install object %s/%s: %w", u.GetKind(), u.GetName(), err)
			}
		}
	}
	return nil
}

func splitInstallDocs(s string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if line == "---" {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	out = append(out, cur.String())
	return out
}

// stopEnvtest tears down the lazily started envtest (managers first, then
// the API server). Safe to call when envtest never started.
func stopEnvtest() {
	if mgrShutdown != nil {
		mgrShutdown()
	}
	if stopRBACPlaneManager != nil {
		stopRBACPlaneManager()
	}
	if testEnv != nil {
		if err := testEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to stop envtest: %v\n", err)
		}
	}
	if rbacAPIEnv != nil {
		if err := rbacAPIEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to stop rbac envtest: %v\n", err)
		}
	}
}

// ensureSingleton makes the fixed-name singleton exist with the given
// mutation applied to the default valid spec. It removes any previous
// singleton first so each test is self-contained.
func ensureSingleton(t *testing.T, mutate func(q *v1alpha1.Quartet)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()

	existing := &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, existing); err == nil {
		if err := testClient.Delete(ctx, existing); err != nil {
			t.Fatalf("delete previous singleton: %v", err)
		}
		waitForDeletion(t, ctx)
	}

	q := validCR()
	if mutate != nil {
		mutate(q)
	}
	if err := testClient.Create(ctx, q); err != nil {
		t.Fatalf("create singleton: %v", err)
	}
}

func waitForDeletion(t *testing.T, ctx context.Context) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, suiteTimeout, true, func(ctx context.Context) (bool, error) {
		q := &v1alpha1.Quartet{}
		if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
			return client.IgnoreNotFound(err) == nil, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("singleton deletion not observed: %v", err)
	}
}

func pollStatus(t *testing.T, name string, check func(*v1alpha1.Quartet) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()
	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, suiteTimeout, true, func(ctx context.Context) (bool, error) {
		q := &v1alpha1.Quartet{}
		if err := testClient.Get(ctx, types.NamespacedName{Name: name}, q); err != nil {
			return false, nil // transient get failure: keep polling
		}
		return check(q), nil
	})
	if err != nil {
		t.Fatalf("condition for %q not reached within %s: %v", name, suiteTimeout, err)
	}
}

func hasCondition(conds []metav1.Condition, condType string, want metav1.ConditionStatus) bool {
	for _, c := range conds {
		if c.Type == condType && c.Status == want {
			return true
		}
	}
	return false
}

// TestEnvtestReconcileValid: CRD apply → CR create → real manager reconcile
// → condition verification against a real API server.
// The first settled condition is WorkloadNotReady (envtest
// runs no kubelet); the test then simulates workload convergence and
// verifies the FULL transition to Ready, including observedGeneration,
// platform resolution and the single-profile status.
func TestEnvtestReconcileValid(t *testing.T) {
	requireEnvtest(t)
	ensureSingleton(t, nil)
	pollWorkloadGate(t)
	convergeWorkloads(t)
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionTrue) &&
			q.Status.ObservedGeneration == q.Generation &&
			q.Status.ResolvedPlatform == v1alpha1.PlatformKubernetes &&
			len(q.Status.Profiles) == 1
	})
}

// TestEnvtestSchemaRejections: negative schema tests against the REAL API
// server — enum, pattern and structural unknown-field rejection.
func TestEnvtestSchemaRejections(t *testing.T) {
	requireEnvtest(t)
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()

	badPlatform := validCR()
	badPlatform.Name = "quartet-bad-platform"
	badPlatform.Spec.Platform = v1alpha1.Platform("windows")
	if err := testClient.Create(ctx, badPlatform); err == nil {
		t.Fatal("platform enum violation must be rejected by the API server")
	}

	badImage := validCR()
	badImage.Name = "quartet-bad-image"
	badImage.Spec.Profiles.NFS.Image.Reference = "quay.io/ngxstorage/nfs:v1.2.3"
	if err := testClient.Create(ctx, badImage); err == nil {
		t.Fatal("image tag without digest must be rejected by the API server")
	}

	badSecret := validCR()
	badSecret.Name = "quartet-bad-secret"
	badSecret.Spec.Profiles.NFS.SecretRef.Name = "Bad Name"
	if err := testClient.Create(ctx, badSecret); err == nil {
		t.Fatal("secretRef.name pattern violation must be rejected by the API server")
	}

	// Unknown fields are rejected by the structural schema
	// (x-kubernetes-preserve-unknown-fields is never set).
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "quartet.ngxstorage.com/v1alpha1",
		"kind":       "Quartet",
		"metadata":   map[string]interface{}{"name": "quartet-unknown-field"},
		"spec": map[string]interface{}{
			"platform":   "auto",
			"bogusField": true,
			"profiles": map[string]interface{}{
				"nfs": map[string]interface{}{"enabled": true, "alsoBogus": "x"},
			},
		},
	}}
	if err := testClient.Create(ctx, u); err != nil {
		t.Fatalf("unexpected create error for unknown-field CR: %v", err)
	}
	// Structural schemas prune unknown fields on write (accepted at admission,
	// dropped before storage): verify the bogus fields never round-tripped.
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "quartet.ngxstorage.com", Version: "v1alpha1", Kind: "Quartet",
	})
	if err := testClient.Get(ctx, types.NamespacedName{Name: "quartet-unknown-field"}, got); err != nil {
		t.Fatalf("get stored object: %v", err)
	}
	spec := got.Object["spec"].(map[string]interface{})
	if _, present := spec["bogusField"]; present {
		t.Fatal("unknown field spec.bogusField survived storage")
	}
	nfs := spec["profiles"].(map[string]interface{})["nfs"].(map[string]interface{})
	if _, present := nfs["alsoBogus"]; present {
		t.Fatal("unknown field spec.profiles.nfs.alsoBogus survived storage")
	}
	// Clean up so later tests see a pristine singleton state.
	if err := testClient.Delete(ctx, got); err != nil {
		t.Fatalf("delete unknown-field CR: %v", err)
	}
}

// TestEnvtestNameMismatchReconcilerSide: the API accepts a foreign name
// (name enforcement is reconciler-side, not CEL); the reconciler must refuse
// with InvalidSpec and produce no profile claims.
func TestEnvtestNameMismatchReconcilerSide(t *testing.T) {
	requireEnvtest(t)
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()

	cr := validCR()
	cr.Name = "quartet-two"
	if err := testClient.Create(ctx, cr); err != nil {
		t.Fatalf("foreign name must be API-accepted (enforcement is reconciler-side): %v", err)
	}
	pollStatus(t, "quartet-two", func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	})
	q := &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: "quartet-two"}, q); err != nil {
		t.Fatal(err)
	}
	if q.Status.Conditions[0].Reason != v1alpha1.ReasonInvalidSpec {
		t.Fatalf("reason = %q, want InvalidSpec", q.Status.Conditions[0].Reason)
	}
	if len(q.Status.Profiles) != 0 {
		t.Fatal("non-singleton must not produce profile status claims")
	}
}

// TestEnvtestEnabledWithoutSecretRef: the conditional requirement
// ("required when enabled") is reconciler-side (no CEL); the API
// accepts the CR, the reconciler reports InvalidSpec.
func TestEnvtestEnabledWithoutSecretRef(t *testing.T) {
	requireEnvtest(t)
	ensureSingleton(t, func(q *v1alpha1.Quartet) {
		q.Spec.Profiles.NFS.SecretRef = nil
	})
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	})
}
