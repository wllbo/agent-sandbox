/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// raceClient simulates the conditions that cause the cross-claim adoption race:
//
//  1. Optimistic concurrency: tracks sandbox resourceVersions and returns
//     409 Conflict when a caller sends a stale resourceVersion.
//  2. Stale informer cache: when enabled, List returns a pre-captured
//     snapshot taken before any mutations, simulating the window where
//     the cache hasn't synced the adoption.
//
// This reproduces the real-world race where two claims see the same stale
// candidate list and both attempt to adopt the same sandbox.
type raceClient struct {
	client.Client

	mu         sync.Mutex
	sandboxRVs map[string]string // live resourceVersions

	staleSnapshot *sandboxv1alpha1.SandboxList
	useStale      atomic.Bool

	updateAttempts atomic.Int32
	patchAttempts  atomic.Int32
	updateConflict atomic.Int32
	patchConflict  atomic.Int32
}

func newRaceClient(inner client.Client) *raceClient {
	return &raceClient{
		Client:     inner,
		sandboxRVs: make(map[string]string),
	}
}

func (c *raceClient) key(obj client.Object) string {
	return obj.GetNamespace() + "/" + obj.GetName()
}

func (c *raceClient) seedRV(obj client.Object) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sandboxRVs[c.key(obj)] = obj.GetResourceVersion()
}

func (c *raceClient) checkAndUpdateRV(obj client.Object) error {
	key := c.key(obj)
	c.mu.Lock()
	defer c.mu.Unlock()
	if liveRV, ok := c.sandboxRVs[key]; ok && obj.GetResourceVersion() != liveRV {
		return k8errors.NewConflict(
			sandboxv1alpha1.Resource("sandboxes"), obj.GetName(),
			fmt.Errorf("resourceVersion %s is stale (current: %s)", obj.GetResourceVersion(), liveRV))
	}
	return nil
}

func (c *raceClient) recordRV(obj client.Object) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sandboxRVs[c.key(obj)] = obj.GetResourceVersion()
}

func (c *raceClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*sandboxv1alpha1.Sandbox); ok {
		c.updateAttempts.Add(1)
		if err := c.checkAndUpdateRV(obj); err != nil {
			c.updateConflict.Add(1)
			return err
		}
		if err := c.Client.Update(ctx, obj, opts...); err != nil {
			return err
		}
		c.recordRV(obj)
		return nil
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c *raceClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if _, ok := obj.(*sandboxv1alpha1.Sandbox); ok {
		c.patchAttempts.Add(1)
		if err := c.checkAndUpdateRV(obj); err != nil {
			c.patchConflict.Add(1)
			return err
		}
		if err := c.Client.Patch(ctx, obj, patch, opts...); err != nil {
			return err
		}
		c.recordRV(obj)
		return nil
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *raceClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if sbList, ok := list.(*sandboxv1alpha1.SandboxList); ok && c.useStale.Load() {
		c.staleSnapshot.DeepCopyInto(sbList)
		return nil
	}
	return c.Client.List(ctx, list, opts...)
}

func (c *raceClient) Status() client.SubResourceWriter {
	return c.Client.Status()
}

// TestCrossClaimConcurrentAdoption reproduces the cross-claim warm pool
// race condition (kubernetes-sigs/agent-sandbox#127).
//
// Setup: 1 warm pool sandbox, 2 claims. After Claim A adopts the sandbox,
// the stale cache is enabled so Claim B's List still shows pool-sb-0 as a
// warm pool candidate. The OCC layer rejects B's attempt to adopt pool-sb-0
// (409 Conflict due to stale resourceVersion).
//
// This test verifies:
//   1. Claim A adopts successfully
//   2. Claim B is blocked from adopting the same sandbox
//   3. Claim B falls through to cold start
//   4. How many wasted API calls Claim B makes (contention cost)
func TestCrossClaimConcurrentAdoption(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()

	poolNameHash := sandboxcontrollers.NameHash("pool")
	templateHash := sandboxcontrollers.NameHash("tpl")
	warmPoolUID := types.UID("pool-uid")

	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tpl", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "pause"}},
				},
			},
		},
	}

	sandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "pool-sb-0",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
			Labels: map[string]string{
				warmPoolSandboxLabel:   poolNameHash,
				sandboxTemplateRefHash: templateHash,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
				Kind:       "SandboxWarmPool",
				Name:       "pool",
				UID:        warmPoolUID,
				Controller: ptr.To(true),
			}},
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			Replicas: ptr.To(int32(1)),
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "pause"}},
				},
			},
		},
		Status: sandboxv1alpha1.SandboxStatus{
			Conditions: []metav1.Condition{{
				Type:               string(sandboxv1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionTrue,
				Reason:             "DependenciesReady",
				LastTransitionTime: metav1.NewTime(time.Now()),
			}},
		},
	}

	claimA := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "claim-a", Namespace: "default", UID: "claim-a-uid",
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "tpl"},
		},
	}

	claimB := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "claim-b", Namespace: "default", UID: "claim-b-uid",
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "tpl"},
		},
	}

	inner := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, sandbox, claimA, claimB).
		WithStatusSubresource(&extensionsv1alpha1.SandboxClaim{}).
		Build()

	rc := newRaceClient(inner)
	rc.seedRV(sandbox)

	// Capture stale snapshot BEFORE any adoption
	var staleSnapshot sandboxv1alpha1.SandboxList
	require.NoError(t, inner.List(ctx, &staleSnapshot, client.InNamespace("default")))
	rc.staleSnapshot = staleSnapshot.DeepCopy()

	reconciler := &SandboxClaimReconciler{
		Client: rc, Scheme: scheme,
		Tracer: asmetrics.NewNoOp(), MaxConcurrentReconciles: 2,
	}

	// --- Claim A reconciles (live cache) ---
	_, err := reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "claim-a", Namespace: "default"},
	})
	require.NoError(t, err)

	// Verify A adopted pool-sb-0
	var sb sandboxv1alpha1.Sandbox
	require.NoError(t, inner.Get(ctx,
		types.NamespacedName{Name: "pool-sb-0", Namespace: "default"}, &sb))
	ref := metav1.GetControllerOf(&sb)
	require.NotNil(t, ref)
	require.Equal(t, types.UID("claim-a-uid"), ref.UID)

	updatesAfterA := rc.updateAttempts.Load()
	patchesAfterA := rc.patchAttempts.Load()
	t.Logf("After Claim A: updates=%d, patches=%d", updatesAfterA, patchesAfterA)

	// --- Enable stale cache for Claim B ---
	// Claim B's List returns the pre-adoption snapshot where pool-sb-0
	// still has warm pool labels and WarmPool owner. This simulates
	// informer cache lag.
	rc.useStale.Store(true)

	// --- Claim B reconciles (stale cache) ---
	_, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "claim-b", Namespace: "default"},
	})
	require.NoError(t, err)

	// Disable stale cache for assertions
	rc.useStale.Store(false)

	// CORE ASSERTION: pool-sb-0 still owned by Claim A
	require.NoError(t, inner.Get(ctx,
		types.NamespacedName{Name: "pool-sb-0", Namespace: "default"}, &sb))
	ref = metav1.GetControllerOf(&sb)
	require.NotNil(t, ref)
	require.Equal(t, types.UID("claim-a-uid"), ref.UID,
		"pool-sb-0 must remain owned by claim-a — claim-b must not steal it")

	// Claim B cold-started its own sandbox
	var coldSb sandboxv1alpha1.Sandbox
	err = inner.Get(ctx, types.NamespacedName{Name: "claim-b", Namespace: "default"}, &coldSb)
	require.NoError(t, err, "claim-b should cold-start a new sandbox")
	coldRef := metav1.GetControllerOf(&coldSb)
	require.NotNil(t, coldRef)
	require.Equal(t, types.UID("claim-b-uid"), coldRef.UID)

	// Contention metrics
	totalUpdates := rc.updateAttempts.Load()
	totalPatches := rc.patchAttempts.Load()
	updateConflicts := rc.updateConflict.Load()
	patchConflicts := rc.patchConflict.Load()
	updatesForB := totalUpdates - updatesAfterA
	patchesForB := totalPatches - patchesAfterA

	t.Logf("Claim B contention: updates=%d (conflicts=%d), patches=%d (conflicts=%d)",
		updatesForB, updateConflicts, patchesForB, patchConflicts)
	t.Logf("Total sandbox API calls: updates=%d, patches=%d", totalUpdates, totalPatches)

	// CONTENTION ASSERTION: Claim B must not make any sandbox Update calls.
	//
	// Each unnecessary r.Update() on a sandbox triggers an ownership transfer,
	// which causes the Sandbox reconciler to delete and recreate the Pod.
	// This is the root cause of "Pod not found" errors under contention:
	//   contention → wasted Update → ownership churn → pod deletion → "Pod not found"
	//
	// On main: Claim B attempts r.Update() on pool-sb-0 (1 wasted Update).
	//   At scale (1500 claims), this produces 1552 "Pod not found" errors.
	//
	// On #599: Claim B's annotation Patch fails first (cheap), so r.Update()
	//   is never attempted on pool-sb-0 (0 wasted Updates, 0 pod errors).
	//
	// This assertion verifies the fix eliminates unnecessary sandbox Updates.
	require.Equal(t, int32(0), updatesForB,
		"Claim B must not call r.Update() on any sandbox — "+
			"each wasted Update triggers pod recreation and 'Pod not found' errors. "+
			"The annotation CAS (Patch) should resolve contention before the Update.")
}
