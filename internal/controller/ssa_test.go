package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/renderer"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"testing"
)

const (
	ssaRaceNS     = "ngxstorage-nfs"
	ssaCreateName = "ssa-create-race-cm"
	ssaUpdateName = "ssa-update-race-cm"
	ssaOwnerUID   = "uid-ssa-test"
)

// ssaWatchClient builds the WithWatch client the interceptors wrap:
// interceptor.NewClient requires the watch surface, which the suite's
// testClient (plain Client) does not declare.
func ssaWatchClient(t *testing.T) client.WithWatch {
	t.Helper()
	ww, err := client.NewWithWatch(testCfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatalf("build WithWatch client: %v", err)
	}
	return ww
}

// cmSnapshot is the full before/after comparison target (condition 2): UID,
// annotations, labels, the data payload and the COMPLETE managedFields list.
type cmSnapshot struct {
	uid         string
	annotations map[string]string
	labels      map[string]string
	data        map[string]string
	managed     []map[string]interface{}
}

func snapshotCM(t *testing.T, c client.Client, key types.NamespacedName) cmSnapshot {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("snapshot get %s: %v", key.Name, err)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	if err := c.Get(context.Background(), key, u); err != nil {
		t.Fatalf("snapshot unstructured get %s: %v", key.Name, err)
	}
	mf, _, _ := unstructured.NestedSlice(u.Object, "metadata", "managedFields")
	managed := make([]map[string]interface{}, 0, len(mf))
	for _, e := range mf {
		m, ok := e.(map[string]interface{})
		if !ok {
			t.Fatalf("managedFields entry %v is not a map", e)
		}
		managed = append(managed, m)
	}
	return cmSnapshot{
		uid:         string(cm.UID),
		annotations: cm.Annotations,
		labels:      cm.Labels,
		data:        cm.Data,
		managed:     managed,
	}
}

func assertSnapshotEqual(t *testing.T, before, after cmSnapshot, what string) {
	t.Helper()
	if before.uid != after.uid {
		t.Fatalf("%s: UID changed (%s -> %s) — the foreign object was replaced", what, before.uid, after.uid)
	}
	if !reflect.DeepEqual(before.annotations, after.annotations) {
		t.Fatalf("%s: annotations changed — an ownership/external annotation was added or stripped:\nbefore=%v\nafter=%v", what, before.annotations, after.annotations)
	}
	if _, has := after.annotations[renderer.OwnerUIDAnnotation]; has {
		t.Fatalf("%s: the foreign object GAINED the quartet ownership annotation (adoption!)", what)
	}
	if !reflect.DeepEqual(before.labels, after.labels) {
		t.Fatalf("%s: labels changed:\nbefore=%v\nafter=%v", what, before.labels, after.labels)
	}
	if !reflect.DeepEqual(before.data, after.data) {
		t.Fatalf("%s: data payload changed:\nbefore=%v\nafter=%v", what, before.data, after.data)
	}
	if !reflect.DeepEqual(before.managed, after.managed) {
		t.Fatalf("%s: managedFields changed — a field manager was added or reassigned:\nbefore=%v\nafter=%v", what, before.managed, after.managed)
	}
}

// TestEnvtestCreateRaceForeignNeverAdopted (create leg, condition 2): the
// ownership gate's GET returns NotFound; AFTER it returns, the interceptor
// creates a foreign object with the SAME name — then the reconciler must
// never adopt it. The atomic CREATE leg is a server-side-apply PATCH carrying
// the impossible sentinel resourceVersion (NOT a POST create); against a
// present object the API server rejects the apply with a 409 Conflict
// (stored RV ≠ sentinel) and writes nothing. The fresh re-read classifies
// the outcome foreignConflict and the object stays byte-identical (UID,
// annotations, data, managedFields — any adoption trace fails the test).
// The sentinel-RV behavior is proven ONLY on this 1.37 envtest plane — it is
// not a normative create-only guarantee on K8s 1.31–1.33 or OCP; live-cluster
// verification is a separate step.
func TestEnvtestCreateRaceForeignNeverAdopted(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	if err := testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ssaRaceNS}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("ensure namespace: %v", err)
	}
	key := types.NamespacedName{Name: ssaCreateName, Namespace: ssaRaceNS}
	_ = testClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ssaCreateName, Namespace: ssaRaceNS}})

	var fired bool
	var before cmSnapshot
	funcs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
			if !fired && k == key {
				fired = true
				// The foreign create lands AFTER the gate's NotFound.
				foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
					Name: ssaCreateName, Namespace: ssaRaceNS,
					Labels:      map[string]string{"managed-by": "user"},
					Annotations: map[string]string{"foreign.example/claim": "user-created"},
				}, Data: map[string]string{"user-key": "user-value"}}
				if err := testClient.Create(ctx, foreign); err != nil {
					t.Errorf("seed foreign object in the race gap: %v", err)
				}
				before = snapshotCM(t, testClient, key)
				// Hide the foreign object from THIS read only: the gate must
				// believe the name is free.
				return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, ssaCreateName)
			}
			return c.Get(ctx, k, obj, opts...)
		},
	}
	a := ssaApplier{c: interceptor.NewClient(ssaWatchClient(t), funcs)}

	want := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: ssaCreateName, Namespace: ssaRaceNS,
		Labels: map[string]string{"quartet": "test"},
	}}
	err := a.applyOwned(ctx, want, ssaOwnerUID)
	var fc *foreignConflict
	if !errors.As(err, &fc) {
		t.Fatalf("create race against a foreign object must classify foreignConflict (never adopt), got %v", err)
	}
	t.Log("PASS: create race classified foreignConflict — adoption not attempted")

	after := snapshotCM(t, testClient, key)
	assertSnapshotEqual(t, before, after, "create-race foreign ConfigMap")
	t.Log("PASS (condition 2): foreign object byte-identical before/after — UID, annotations, data and managedFields unchanged")
}

// TestEnvtestUpdateRaceStaleRVPrecondition (update leg, condition 1):
//
// - leg A (RAW rejection): an SSA apply carrying a manually STALE
// resourceVersion is rejected by the API SERVER itself with a 409
// conflict — observed directly, proving the precondition is enforced by
// the server, not by client-side bookkeeping;
// - leg B (BODY carry): an interceptor captures the serialized SSA request
// body and asserts its metadata.resourceVersion equals the gate's value
// on EVERY attempt;
// - leg C (bounded retry converges): with the gate's
// read stale (the interceptor serves it after a BENIGN external edit
// landed), applyOwned's first apply 409s, the fresh re-read verifies the
// object is still ours (UID + annotation), and the SECOND attempt on the
// fresh RV converges — nil error, the external field survives, our
// fields intact. A still-owned conflict is operational, never terminal.
func TestEnvtestUpdateRaceStaleRVPrecondition(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	if err := testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ssaRaceNS}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("ensure namespace: %v", err)
	}
	key := types.NamespacedName{Name: ssaUpdateName, Namespace: ssaRaceNS}
	_ = testClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ssaUpdateName, Namespace: ssaRaceNS}})

	// An OWNED object: the gate will accept it (annotation + UID match).
	owned := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: ssaUpdateName, Namespace: ssaRaceNS,
		Annotations: map[string]string{renderer.OwnerUIDAnnotation: ssaOwnerUID},
		Labels:      map[string]string{"quartet": "ours"},
	}, Data: map[string]string{"our-key": "our-value"}}
	if err := testClient.Create(ctx, owned); err != nil {
		t.Fatalf("seed owned ConfigMap: %v", err)
	}

	// gate view (stale): the state BEFORE the external edit.
	gate := &corev1.ConfigMap{}
	if err := testClient.Get(ctx, key, gate); err != nil {
		t.Fatalf("gate read: %v", err)
	}
	gateRV := gate.ResourceVersion
	gateCopy := gate.DeepCopy()

	// external edit: an annotation we do not own bumps the stored RV.
	external := &corev1.ConfigMap{}
	if err := testClient.Get(ctx, key, external); err != nil {
		t.Fatal(err)
	}
	external.Annotations["external.example/race"] = "landed-in-gap"
	if err := testClient.Update(ctx, external); err != nil {
		t.Fatalf("external edit: %v", err)
	}
	if external.ResourceVersion == gateRV {
		t.Fatal("precondition: external edit must bump the stored resourceVersion")
	}

	// --- leg A: RAW stale-RV apply rejected by the API server. ---
	staleApply := external.DeepCopy()
	staleApply.ResourceVersion = gateRV // manually stale
	staleApply.ManagedFields = nil      // SSA rejects a body carrying managedFields; production applies a clean rendered object
	staleApply.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	raw := ssaApplier{c: testClient}
	aerr := raw.applyWithoutForce(ctx, staleApply)
	if !apierrors.IsConflict(aerr) {
		t.Fatalf("leg A: stale-RV SSA must be rejected by the API server with a 409 conflict, got %v", aerr)
	}
	t.Logf("PASS leg A: API server rejected the stale-RV apply: %v", aerr)

	// --- leg B + C: through the reconciler, with the gate's read stale. ---
	var fired bool
	var bodyRVs []string
	var attempts int
	funcs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
			if !fired && k == key {
				fired = true
				// Serve the STALE gate view ONLY for the gate's read: the
				// external edit is invisible to the gate, exactly as in the
				// live race. The conflict's fresh re-reads pass through.
				if cm, ok := obj.(*corev1.ConfigMap); ok {
					*cm = *gateCopy
					return nil
				}
			}
			return c.Get(ctx, k, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetName() == key.Name && obj.GetNamespace() == key.Namespace && patch.Type() == types.ApplyPatchType {
				attempts++
				// The apply body is json.Marshal(obj) (controller-runtime
				// applyPatch.Data) — serialize it and read the body's own
				// metadata.resourceVersion.
				body, merr := json.Marshal(obj)
				if merr != nil {
					t.Fatalf("serialize apply body: %v", merr)
				}
				var doc map[string]interface{}
				if merr := json.Unmarshal(body, &doc); merr != nil {
					t.Fatalf("unmarshal apply body: %v", merr)
				}
				meta, _ := doc["metadata"].(map[string]interface{})
				bodyRVs = append(bodyRVs, getString(meta, "resourceVersion"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
	a := ssaApplier{c: interceptor.NewClient(ssaWatchClient(t), funcs)}
	// A CLEAN rendered-style desired object: production applies the renderer's
	// output, which has never round-tripped the API and therefore carries no
	// managedFields (SSA rejects a body that does). Reusing the API version
	// of `owned` would inject fields the reconciler never sends.
	want := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: ssaUpdateName, Namespace: ssaRaceNS,
		Labels: map[string]string{"quartet": "ours"},
	}, Data: map[string]string{"our-key": "our-value"}}
	err := a.applyOwned(ctx, want, ssaOwnerUID)
	var fc *foreignConflict
	if errors.As(err, &fc) {
		t.Fatalf("leg C: a benign concurrent edit on an OWNED object must NOT classify foreignConflict, got %v", err)
	}
	if err != nil {
		t.Fatalf("leg C: bounded retry on fresh state must converge the benign race, got %v", err)
	}
	t.Log("PASS leg C: benign concurrent edit converged — no foreignConflict, bounded retry returned success")

	// leg B: the FIRST attempt carried the gate's (stale) RV; the RETRY
	// carried the fresh RV from the re-read — the precondition advanced.
	if len(bodyRVs) != 2 || attempts != 2 {
		t.Fatalf("expected exactly 2 apply attempts (stale, then retried on fresh RV), got %d attempts / %d bodies", attempts, len(bodyRVs))
	}
	if bodyRVs[0] != gateRV {
		t.Fatalf("leg B: first SSA body's metadata.resourceVersion = %q, want the gate's %q", bodyRVs[0], gateRV)
	}
	if bodyRVs[1] == gateRV {
		t.Fatalf("leg B: the retried SSA body must carry the FRESH resourceVersion, still had the gate's %q", gateRV)
	}
	t.Logf("PASS leg B: first SSA body carried the gate's RV %s; the retry carried the fresh RV %s", gateRV, bodyRVs[1])

	// The object converged: the external field SURVIVES (never force-taken),
	// our fields are present (the retry landed).
	after := &corev1.ConfigMap{}
	if err := testClient.Get(ctx, key, after); err != nil {
		t.Fatal(err)
	}
	if after.Annotations["external.example/race"] != "landed-in-gap" {
		t.Fatalf("the external field must SURVIVE the bounded retry (no force), got %v", after.Annotations)
	}
	if after.Labels["quartet"] != "ours" || after.Data["our-key"] != "our-value" {
		t.Fatalf("our fields must converge after the retry, got labels=%v data=%v", after.Labels, after.Data)
	}
}

func getString(m map[string]interface{}, k string) string {
	v, _ := m[k].(string)
	return v
}

// TestEnvtestBenignConcurrentEditConvergesNoEvents (test
// (a) at the RECONCILE level): an unrelated metadata concurrent edit (a
// benign annotation the operator never declares) lands, and then a DIRECT
// reconcile call runs with NO watch event in between — the edit's own event
// may already have been consumed in-flight, so "a later event will come" is
// not a recovery guarantee. The stable owned object must converge: the
// reconciler's stale gate read 409s, the bounded retry re-verifies ownership
// (UID + annotation intact — ours) and re-applies on the fresh RV; the
// external field survives, our fields stay, and no conflict condition
// surfaces.
func TestEnvtestBenignConcurrentEditConvergesNoEvents(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	if err := testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ssaRaceNS}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("ensure namespace: %v", err)
	}
	key := types.NamespacedName{Name: ssaUpdateName, Namespace: ssaRaceNS}
	_ = testClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ssaUpdateName, Namespace: ssaRaceNS}})

	// Establish ownership with a real SSA create (operation:Apply from the
	// first write — the production steady state).
	a := ssaApplier{c: testClient}
	want := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: ssaUpdateName, Namespace: ssaRaceNS,
		Labels: map[string]string{"quartet": "ours"},
	}, Data: map[string]string{"our-key": "our-value"}}
	if err := a.applyOwned(ctx, want, ssaOwnerUID); err != nil {
		t.Fatalf("initial SSA apply: %v", err)
	}
	// NOTE: controller-runtime's SSA decodes the server response (managed
	// fields + real RV) back into the applied object, so `want` is now
	// polluted — production instead renders a FRESH desired object every
	// reconcile, and the race apply below uses a clean one.

	// Unrelated metadata concurrency: a benign annotation bumping the RV.
	gateView := &corev1.ConfigMap{}
	if err := testClient.Get(ctx, key, gateView); err != nil {
		t.Fatal(err)
	}
	edited := gateView.DeepCopy()
	if edited.Annotations == nil {
		edited.Annotations = map[string]string{}
	}
	edited.Annotations["external.example/benign"] = "landed-in-gap"
	if err := testClient.Update(ctx, edited); err != nil {
		t.Fatalf("benign external edit: %v", err)
	}

	// Serve the STALE gate view for the reconciler's ownership-gate read of
	// THIS object only; every other call hits the real API. No event has
	// fired — a direct applyOwned call stands in for the eventless
	// reconcile.
	var staleServed bool
	var patchAttempts int
	funcs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
			if !staleServed && k == key {
				staleServed = true
				if cm, ok := obj.(*corev1.ConfigMap); ok {
					*cm = *gateView
					return nil
				}
			}
			return c.Get(ctx, k, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetName() == key.Name && obj.GetNamespace() == key.Namespace {
				patchAttempts++
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
	// A CLEAN rendered-style desired object — exactly what production passes
	// to the update leg (the first `want` was polluted by the create leg's
	// SSA response round-trip; an SSA body carrying managedFields is
	// rejected outright).
	freshWant := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: ssaUpdateName, Namespace: ssaRaceNS,
		Labels: map[string]string{"quartet": "ours"},
	}, Data: map[string]string{"our-key": "our-value"}}
	a = ssaApplier{c: interceptor.NewClient(ssaWatchClient(t), funcs)}
	err := a.applyOwned(ctx, freshWant, ssaOwnerUID)
	if err != nil {
		t.Fatalf("benign concurrent edit with no follow-up event must converge via bounded retry, got %v", err)
	}
	if patchAttempts < 2 {
		t.Fatalf("expected the stale first attempt to 409 and the retry to land (attempts=%d)", patchAttempts)
	}

	after := &corev1.ConfigMap{}
	if err := testClient.Get(ctx, key, after); err != nil {
		t.Fatal(err)
	}
	if after.Annotations["external.example/benign"] != "landed-in-gap" {
		t.Fatalf("the benign external field must SURVIVE convergence (no force), got %v", after.Annotations)
	}
	if after.Labels["quartet"] != "ours" || after.Data["our-key"] != "our-value" {
		t.Fatalf("our fields must be intact after convergence, got labels=%v data=%v", after.Labels, after.Data)
	}
	if after.GetAnnotations()[renderer.OwnerUIDAnnotation] != ssaOwnerUID {
		t.Fatalf("ownership must survive convergence, got %v", after.Annotations)
	}
	t.Log("PASS: eventless benign concurrency converged — stable owned object repaired within the bounded retry")
}

// seedOwnedCM establishes a clean SSA-created, owned ConfigMap (the
// update-leg probe object, same plane).
func seedOwnedCM(t *testing.T, key types.NamespacedName) {
	t.Helper()
	ctx := context.Background()
	_ = testClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}})
	a := ssaApplier{c: testClient}
	want := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      key.Name,
		Namespace: key.Namespace,
		Labels:    map[string]string{"quartet": "ours"},
	}, Data: map[string]string{"our-key": "our-value"}}
	if err := a.applyOwned(ctx, want, ssaOwnerUID); err != nil {
		t.Fatalf("seed owned ConfigMap: %v", err)
	}
}

// cleanRenderedCM is a rendered-style desired object (no managedFields —
// SSA rejects a body carrying them).
func cleanRenderedCM(name, ns string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: ns,
		Labels:    map[string]string{"quartet": "ours"},
	}, Data: map[string]string{"our-key": "our-value"}}
}

// TestEnvtestSustainedBenignConcurrencyNoForeignDiagnosis (applyOwned
// level): a concurrent benign writer lands in EVERY
// read→apply gap, so all updateConflictRetries attempts 409 on the RV
// precondition with no causes. The bounded bound exhausts — and the
// outcome must NOT be a foreignConflict diagnosis: the raw RV conflict is
// returned, classified retryable.
func TestEnvtestSustainedBenignConcurrencyNoForeignDiagnosis(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	if err := testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ssaRaceNS}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("ensure namespace: %v", err)
	}
	key := types.NamespacedName{Name: ssaUpdateName, Namespace: ssaRaceNS}
	seedOwnedCM(t, key)

	armed := true
	var attempts int
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if armed && patch.Type() == types.ApplyPatchType &&
				obj.GetName() == key.Name && obj.GetNamespace() == key.Namespace {
				attempts++
				// A benign concurrent write in this attempt's gap: bump an
				// annotation (RV moves; UID + ownership annotation intact).
				live := &corev1.ConfigMap{}
				if err := testClient.Get(ctx, key, live); err != nil {
					t.Errorf("benign race write: %v", err)
				}
				if live.Annotations == nil {
					live.Annotations = map[string]string{}
				}
				// A unique value per attempt: a no-op Update does NOT bump
				// the stored resourceVersion, and the bump is the whole
				// point of the race.
				live.Annotations["external.example/sustained"] = fmt.Sprintf("racing-%d", attempts)
				_ = testClient.Update(ctx, live)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
	a := ssaApplier{c: interceptor.NewClient(ssaWatchClient(t), funcs)}
	err := a.applyOwned(ctx, cleanRenderedCM(key.Name, key.Namespace), ssaOwnerUID)
	if err == nil {
		t.Fatal("sustained concurrency must not converge inside the bound")
	}
	if attempts < updateConflictRetries {
		t.Fatalf("expected the bound to be consumed (attempts=%d, want >= %d)", attempts, updateConflictRetries)
	}
	var fc *foreignConflict
	if errors.As(err, &fc) {
		t.Fatalf("sustained benign RV concurrency must NOT be diagnosed as foreign field ownership (bound exhaustion is not evidence), got %v", err)
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected the raw RV conflict to propagate for retryable classification, got %v", err)
	}
	if isFieldOwnershipConflict(err) {
		t.Fatalf("the API reported conflict CAUSES on a benign RV race — the cause discriminator is wrong: %v", err)
	}
	reason, retryable := classifyApplyError(err)
	if reason != v1alpha1.ReasonApplyConflict || !retryable {
		t.Fatalf("exhausted RV-only conflicts must classify (ApplyConflict, retryable), got (%q %v)", reason, retryable)
	}
	t.Log("PASS: sustained benign concurrency exhausted the bound WITHOUT a foreign-ownership diagnosis — retryable requeue")
}

// TestEnvtestSustainedBenignConcurrencyRequeuesThenConverges (RECONCILE
// level): with a rendered operand's apply perpetually racing a
// benign writer, a full reconcile must requeue (ApplyConflict, retryable)
// — never surface ForeignResourceConflict — and converge on the next pass
// once the concurrency stops.
func TestEnvtestSustainedBenignConcurrencyRequeuesThenConverges(t *testing.T) {
	requireEnvtest(t)
	ensureSingleton(t, nil)
	pollWorkloadGate(t) // operands applied

	coordCM := types.NamespacedName{Name: "nfs-csi-ngxstorage-coordination-config", Namespace: envtestNFSNS}
	// The armed reconcile must take the UPDATE leg (owned + present), so pin
	// the precondition: the coordination CM exists and carries this CR's
	// owner annotation. Otherwise an event-driven create leg could race in
	// before the arm and the interceptor's bump would be a no-op.
	q0 := &v1alpha1.Quartet{}
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, q0); err != nil {
		t.Fatal(err)
	}
	pollObj(t, func(ctx context.Context) bool {
		cm := &corev1.ConfigMap{}
		if err := testClient.Get(ctx, coordCM, cm); err != nil {
			return false
		}
		return cm.Annotations[renderer.OwnerUIDAnnotation] == string(q0.UID)
	})
	armed := true
	var armAttempts int
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if armed && patch.Type() == types.ApplyPatchType &&
				obj.GetName() == coordCM.Name && obj.GetNamespace() == coordCM.Namespace {
				armAttempts++
				live := &corev1.ConfigMap{}
				if err := testClient.Get(ctx, coordCM, live); err == nil {
					if live.Annotations == nil {
						live.Annotations = map[string]string{}
					}
					// Unique per attempt: a no-op Update does not bump the
					// stored resourceVersion, and the bump is the race.
					live.Annotations["external.example/sustained"] = fmt.Sprintf("racing-%d", armAttempts)
					_ = testClient.Update(ctx, live)
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
	wrapped, err := client.NewWithWatch(testCfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatal(err)
	}
	// Swap the reconciler's safety-critical client to the intercepted one;
	// applierOrDefault() builds the SSA applier over freshClient().
	origDirect, origClient := testReconciler.DirectClient, testReconciler.Client
	origApplier := testReconciler.Applier
	testReconciler.DirectClient = interceptor.NewClient(wrapped, funcs)
	testReconciler.Client = interceptor.NewClient(wrapped, funcs)
	testReconciler.Applier = nil
	defer func() {
		testReconciler.DirectClient, testReconciler.Client, testReconciler.Applier = origDirect, origClient, origApplier
	}()

	ctx := context.Background()
	res, rerr := testReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: v1alpha1.QuartetName}})
	if rerr != nil {
		t.Fatalf("raced reconcile error: %v", rerr)
	}
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("sustained benign race must requeue retryable, got %+v", res)
	}
	q := &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	c := condition(t, q.Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason == v1alpha1.ReasonForeignResourceConflict {
		t.Fatalf("ForeignResourceConflict surfaced on sustained benign concurrency (message %s) — false ownership diagnosis", c.Message)
	}
	if c.Reason != v1alpha1.ReasonApplyConflict {
		t.Fatalf("raced profile reason = %q, want ApplyConflict", c.Reason)
	}
	t.Log("PASS: sustained benign race → ApplyConflict retryable requeue, no ForeignResourceConflict")

	// The concurrency settles: the next pass repairs the operand. Converge
	// the workload gate too so the pass has no other retryable left.
	armed = false
	convergeWorkloads(t)
	res, rerr = testReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: v1alpha1.QuartetName}})
	if rerr != nil {
		t.Fatalf("post-settle reconcile error: %v", rerr)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("post-settle reconcile must converge, got %+v", res)
	}
	gotCM := &corev1.ConfigMap{}
	if err := testClient.Get(ctx, coordCM, gotCM); err != nil {
		t.Fatal(err)
	}
	if gotCM.Annotations[renderer.OwnerUIDAnnotation] != string(q.UID) {
		t.Fatalf("ownership annotation mismatch after convergence: %v", gotCM.Annotations)
	}
	t.Log("PASS: convergence after the race cleared — the retryable path repaired the operand")
}

// TestEnvtestFieldStealTerminalNoForceConflict (terminal half):
// a REAL field steal — another manager SSA-applies the SAME field — makes
// the API report conflict CAUSES. The outcome must be terminal
// foreignConflict IMMEDIATELY (the first cause-bearing conflict, not after
// the bound), the foreign field survives, and no force repair happens.
func TestEnvtestFieldStealTerminalNoForceConflict(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	if err := testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ssaRaceNS}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("ensure namespace: %v", err)
	}
	key := types.NamespacedName{Name: ssaUpdateName, Namespace: ssaRaceNS}
	// Seed WITHOUT labels: our apply declares labels.quartet, so the contested
	// field must be FREE for the steal below to land (evil-manager claims it
	// first); a pre-existing SSA owner would make the steal itself conflict.
	_ = testClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}})
	seed := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:        key.Name,
		Namespace:   key.Namespace,
		Annotations: map[string]string{renderer.OwnerUIDAnnotation: ssaOwnerUID},
	}, Data: map[string]string{"our-key": "our-value"}}
	if err := testClient.Create(ctx, seed); err != nil {
		t.Fatalf("seed unlabelled owned ConfigMap: %v", err)
	}

	armed := true
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if armed && patch.Type() == types.ApplyPatchType &&
				obj.GetName() == key.Name && obj.GetNamespace() == key.Namespace {
				// A real field steal: evil-manager SSA-claims the SAME label
				// key our apply declares — the API server then rejects our
				// no-force apply with causes (FieldValueConflict).
				evil := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
					Name:      key.Name,
					Namespace: key.Namespace,
					Labels:    map[string]string{"quartet": "stolen"},
				}}
				evil.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
				if err := testClient.Patch(ctx, evil, client.Apply, client.FieldOwner("evil-manager")); err != nil {
					t.Errorf("seed field steal: %v", err)
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
	// Count attempts including the steal: the steal Patch also hits the
	// interceptor (FieldOwner applies are types.ApplyPatchType), so count
	// via the delegate only.
	var delegatePatches int
	origFuncs := funcs
	funcs = interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			delegatePatches++
			return origFuncs.Patch(ctx, c, obj, patch, opts...)
		},
	}
	a := ssaApplier{c: interceptor.NewClient(ssaWatchClient(t), funcs)}
	// Our apply declares ONLY the contested label — no other field is
	// contested, so the conflict (if any) is precisely the steal.
	want := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      key.Name,
		Namespace: key.Namespace,
		Labels:    map[string]string{"quartet": "ours"},
	}}
	err := a.applyOwned(ctx, want, ssaOwnerUID)
	var fc *foreignConflict
	if !errors.As(err, &fc) {
		var se *apierrors.StatusError
		detail := "(not a StatusError)"
		if errors.As(err, &se) && se.ErrStatus.Details != nil {
			detail = fmt.Sprintf("reason=%s causes=%v", se.ErrStatus.Reason, se.ErrStatus.Details.Causes)
		} else if errors.As(err, &se) {
			detail = fmt.Sprintf("reason=%s (no details)", se.ErrStatus.Reason)
		}
		t.Fatalf("a real field steal must classify terminal foreignConflict, got (%T) %v — %s", err, err, detail)
	}
	if delegatePatches > 1 {
		t.Fatalf("a cause-bearing conflict must be terminal IMMEDIATELY — the bound is moot (attempts=%d)", delegatePatches)
	}
	// The foreign field survives exactly as written; our apply did not
	// force-repair it.
	got := &corev1.ConfigMap{}
	if err := testClient.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Labels["quartet"] != "stolen" {
		t.Fatalf("the foreign field must survive (no force), labels=%v", got.Labels)
	}
	if got.Annotations[renderer.OwnerUIDAnnotation] != ssaOwnerUID {
		t.Fatalf("ownership annotation must be intact (the object was never replaced): %v", got.Annotations)
	}
	t.Logf("PASS: real field steal → terminal foreignConflict on the FIRST conflict (causes present), foreign field survived; detail: %s", fc.Error())
}
