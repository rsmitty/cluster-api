/*
Copyright 2019 The Kubernetes Authors.

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
	"reflect"
	"strings"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/pointer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	"sigs.k8s.io/cluster-api/controllers/external"
	capierrors "sigs.k8s.io/cluster-api/errors"
	expv1 "sigs.k8s.io/cluster-api/exp/api/v1alpha3"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var (
	externalReadyWait = 30 * time.Second
)

func (r *MachinePoolReconciler) reconcilePhase(mp *expv1.MachinePool) {
	// Set the phase to "pending" if nil.
	if mp.Status.Phase == "" {
		mp.Status.SetTypedPhase(expv1.MachinePoolPhasePending)
	}

	// Set the phase to "provisioning" if bootstrap is ready and the infrastructure isn't.
	if mp.Status.BootstrapReady && !mp.Status.InfrastructureReady {
		mp.Status.SetTypedPhase(expv1.MachinePoolPhaseProvisioning)
	}

	// Set the phase to "provisioned" if the infrastructure is ready.
	if len(mp.Status.NodeRefs) != 0 {
		mp.Status.SetTypedPhase(expv1.MachinePoolPhaseProvisioned)
	}

	// Set the phase to "running" if the number of ready replicas is equal to desired replicas.
	if mp.Status.InfrastructureReady && *mp.Spec.Replicas == mp.Status.ReadyReplicas {
		mp.Status.SetTypedPhase(expv1.MachinePoolPhaseRunning)
	}

	// Set the phase to "scalingUp" if the infrastructure is scaling up.
	if mp.Status.InfrastructureReady && *mp.Spec.Replicas > mp.Status.ReadyReplicas {
		mp.Status.SetTypedPhase(expv1.MachinePoolPhaseScalingUp)
	}

	// Set the phase to "scalingDown" if the infrastructure is scaling down.
	if mp.Status.InfrastructureReady && *mp.Spec.Replicas < mp.Status.ReadyReplicas {
		mp.Status.SetTypedPhase(expv1.MachinePoolPhaseScalingDown)
	}

	// Set the phase to "failed" if any of Status.FailureReason or Status.FailureMessage is not-nil.
	if mp.Status.FailureReason != nil || mp.Status.FailureMessage != nil {
		mp.Status.SetTypedPhase(expv1.MachinePoolPhaseFailed)
	}

	// Set the phase to "deleting" if the deletion timestamp is set.
	if !mp.DeletionTimestamp.IsZero() {
		mp.Status.SetTypedPhase(expv1.MachinePoolPhaseDeleting)
	}
}

// reconcileExternal handles generic unstructured objects referenced by a MachinePool.
func (r *MachinePoolReconciler) reconcileExternal(ctx context.Context, cluster *clusterv1.Cluster, m *expv1.MachinePool, ref *corev1.ObjectReference) (external.ReconcileOutput, error) {
	logger := r.Log.WithValues("machinepool", m.Name, "namespace", m.Namespace)

	obj, err := external.Get(ctx, r.Client, ref, m.Namespace)
	if err != nil {
		if apierrors.IsNotFound(errors.Cause(err)) {
			return external.ReconcileOutput{}, errors.Wrapf(&capierrors.RequeueAfterError{RequeueAfter: externalReadyWait},
				"could not find %v %q for MachinePool %q in namespace %q, requeuing",
				ref.GroupVersionKind(), ref.Name, m.Name, m.Namespace)
		}
		return external.ReconcileOutput{}, err
	}

	// if external ref is paused, return error.
	if annotations.IsPaused(cluster, obj) {
		logger.V(3).Info("External object referenced is paused")
		return external.ReconcileOutput{Paused: true}, nil
	}

	// Initialize the patch helper.
	patchHelper, err := patch.NewHelper(obj, r.Client)
	if err != nil {
		return external.ReconcileOutput{}, err
	}

	// Set external object ControllerReference to the MachinePool.
	if err := controllerutil.SetControllerReference(m, obj, r.scheme); err != nil {
		return external.ReconcileOutput{}, err
	}

	// Set the Cluster label.
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[clusterv1.ClusterLabelName] = m.Spec.ClusterName
	obj.SetLabels(labels)

	// Always attempt to Patch the external object.
	if err := patchHelper.Patch(ctx, obj); err != nil {
		return external.ReconcileOutput{}, err
	}

	// Add watcher for external object, if there isn't one already.
	_, loaded := r.externalWatchers.LoadOrStore(obj.GroupVersionKind().String(), struct{}{})
	if !loaded && r.controller != nil {
		logger.Info("Adding watcher on external object", "gvk", obj.GroupVersionKind())
		err := r.controller.Watch(
			&source.Kind{Type: obj},
			&handler.EnqueueRequestForOwner{OwnerType: &expv1.MachinePool{}},
		)
		if err != nil {
			r.externalWatchers.Delete(obj.GroupVersionKind().String())
			return external.ReconcileOutput{}, errors.Wrapf(err, "failed to add watcher on external object %q", obj.GroupVersionKind())
		}
	}

	// Set failure reason and message, if any.
	failureReason, failureMessage, err := external.FailuresFrom(obj)
	if err != nil {
		return external.ReconcileOutput{}, err
	}
	if failureReason != "" {
		machineStatusFailure := capierrors.MachinePoolStatusFailure(failureReason)
		m.Status.FailureReason = &machineStatusFailure
	}
	if failureMessage != "" {
		m.Status.FailureMessage = pointer.StringPtr(
			fmt.Sprintf("Failure detected from referenced resource %v with name %q: %s",
				obj.GroupVersionKind(), obj.GetName(), failureMessage),
		)
	}

	return external.ReconcileOutput{Result: obj}, nil
}

// reconcileBootstrap reconciles the Spec.Bootstrap.ConfigRef object on a MachinePool.
func (r *MachinePoolReconciler) reconcileBootstrap(ctx context.Context, cluster *clusterv1.Cluster, m *expv1.MachinePool) error {
	// Call generic external reconciler if we have an external reference.
	var bootstrapConfig *unstructured.Unstructured
	if m.Spec.Template.Spec.Bootstrap.ConfigRef != nil {
		bootstrapReconcileResult, err := r.reconcileExternal(ctx, cluster, m, m.Spec.Template.Spec.Bootstrap.ConfigRef)
		if err != nil {
			return err
		}
		// if the external object is paused, return without any further processing
		if bootstrapReconcileResult.Paused {
			return nil
		}
		bootstrapConfig = bootstrapReconcileResult.Result
	}

	// If the bootstrap data secret is populated, set ready and return.
	if m.Spec.Template.Spec.Bootstrap.Data != nil || m.Spec.Template.Spec.Bootstrap.DataSecretName != nil {
		m.Status.BootstrapReady = true
		return nil
	}

	// If the bootstrap config is being deleted, return early.
	if !bootstrapConfig.GetDeletionTimestamp().IsZero() {
		return nil
	}

	// Determine if the bootstrap provider is ready.
	ready, err := external.IsReady(bootstrapConfig)
	if err != nil {
		return err
	} else if !ready {
		return errors.Wrapf(&capierrors.RequeueAfterError{RequeueAfter: externalReadyWait},
			"Bootstrap provider for MachinePool %q in namespace %q is not ready, requeuing", m.Name, m.Namespace)
	}

	// Get and set the name of the secret containing the bootstrap data.
	secretName, _, err := unstructured.NestedString(bootstrapConfig.Object, "status", "dataSecretName")
	if err != nil {
		return errors.Wrapf(err, "failed to retrieve dataSecretName from bootstrap provider for MachinePool %q in namespace %q", m.Name, m.Namespace)
	} else if secretName == "" {
		return errors.Errorf("retrieved empty dataSecretName from bootstrap provider for MachinePool %q in namespace %q", m.Name, m.Namespace)
	}

	m.Spec.Template.Spec.Bootstrap.DataSecretName = pointer.StringPtr(secretName)
	m.Status.BootstrapReady = true
	return nil
}

// reconcileInfrastructure reconciles the Spec.InfrastructureRef object on a MachinePool.
func (r *MachinePoolReconciler) reconcileInfrastructure(ctx context.Context, cluster *clusterv1.Cluster, mp *expv1.MachinePool) error {
	// Call generic external reconciler.
	infraReconcileResult, err := r.reconcileExternal(ctx, cluster, mp, &mp.Spec.Template.Spec.InfrastructureRef)
	if err != nil {
		if mp.Status.InfrastructureReady && strings.Contains(err.Error(), "could not find") {
			// Infra object went missing after the machine pool was up and running
			r.Log.Error(err, "MachinePool infrastructure reference has been deleted after being ready, setting failure state")
			mp.Status.FailureReason = capierrors.MachinePoolStatusErrorPtr(capierrors.InvalidConfigurationMachinePoolError)
			mp.Status.FailureMessage = pointer.StringPtr(fmt.Sprintf("MachinePool infrastructure resource %v with name %q has been deleted after being ready",
				mp.Spec.Template.Spec.InfrastructureRef.GroupVersionKind(), mp.Spec.Template.Spec.InfrastructureRef.Name))
		}
		return err
	}
	// if the external object is paused, return without any further processing
	if infraReconcileResult.Paused {
		return nil
	}
	infraConfig := infraReconcileResult.Result

	if !infraConfig.GetDeletionTimestamp().IsZero() {
		return nil
	}

	ready, err := external.IsReady(infraConfig)
	if err != nil {
		return err
	}

	mp.Status.InfrastructureReady = ready
	if !mp.Status.InfrastructureReady {
		return errors.Wrapf(&capierrors.RequeueAfterError{RequeueAfter: externalReadyWait},
			"Infrastructure provider for MachinePool %q in namespace %q is not ready, requeuing", mp.Name, mp.Namespace,
		)
	}

	var providerIDList []string
	// Get Spec.ProviderIDList from the infrastructure provider.
	if err := util.UnstructuredUnmarshalField(infraConfig, &providerIDList, "spec", "providerIDList"); err != nil {
		return errors.Wrapf(err, "failed to retrieve data from infrastructure provider for MachinePool %q in namespace %q", mp.Name, mp.Namespace)
	} else if len(providerIDList) == 0 {
		return errors.Wrapf(&capierrors.RequeueAfterError{RequeueAfter: externalReadyWait},
			"retrieved empty Spec.ProviderIDList from infrastructure provider for MachinePool %q in namespace %q", mp.Name, mp.Namespace,
		)
	}

	// Get and set Status.Replicas from the infrastructure provider.
	err = util.UnstructuredUnmarshalField(infraConfig, &mp.Status.Replicas, "status", "replicas")
	if err != nil {
		if err != util.ErrUnstructuredFieldNotFound {
			return errors.Wrapf(err, "failed to retrieve replicas from infrastructure provider for MachinePool %q in namespace %q", mp.Name, mp.Namespace)
		}
	} else if mp.Status.Replicas == 0 {
		return errors.Wrapf(&capierrors.RequeueAfterError{RequeueAfter: externalReadyWait},
			"retrieved unset Status.Replicas from infrastructure provider for MachinePool %q in namespace %q", mp.Name, mp.Namespace,
		)
	}

	if !reflect.DeepEqual(mp.Spec.ProviderIDList, providerIDList) {
		mp.Spec.ProviderIDList = providerIDList
		mp.Status.ReadyReplicas = 0
		mp.Status.AvailableReplicas = 0
		mp.Status.UnavailableReplicas = mp.Status.Replicas
	}

	return nil
}
