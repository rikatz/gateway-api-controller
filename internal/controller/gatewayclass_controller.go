/*
Copyright 2023.

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

package controller

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"sigs.k8s.io/controller-runtime/pkg/log"

	gatewayapis "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// GatewayClassReconciler reconciles a GatewayClass object
type GatewayClassReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	ControllerName string
}

//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/finalizers,verbs=update

// Reconcile reconciles a GatewayClass
func (r *GatewayClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.WithValues("controller", req.Name)
	logger.Info("reconciling class")

	var err error
	var class gatewayapis.GatewayClass
	if err = r.Get(ctx, req.NamespacedName, &class); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "unable to fetch GatewayClass")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !class.ObjectMeta.DeletionTimestamp.IsZero() {
		// TODO: This finalizer should be added only if there are gateway objects pointing to this class. It also should be
		// removed just when there are no more classes pointing to this class
		if controllerutil.ContainsFinalizer(&class, gatewayapis.GatewayClassFinalizerGatewaysExist) {
			logger.Info("Removing finalizer from object")
			controllerutil.RemoveFinalizer(&class, gatewayapis.GatewayClassFinalizerGatewaysExist)
			if err := r.Update(ctx, &class); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Setting it as Unknown for future usages. Right now GatewayClass can or cannot be accepted :)
	condition := metav1.ConditionUnknown
	if class.Spec.ParametersRef != nil {
		// TODO: The defined parameter group should be added in a future to a queue. In case it changes, we should
		// trigger a new gatewayclass reconciliation
		paramRef := class.Spec.ParametersRef
		condition = metav1.ConditionFalse
		logger.Info("should configure extra parameters", "group", paramRef.Group, "kind", paramRef.Kind,
			"name", paramRef.Name, "namespace", paramRef.Namespace)
		if paramRef.Group == "v1" && paramRef.Kind != "ConfigMap" {
			return r.setClassCondition(ctx, req, gatewayapis.GatewayClassReasonInvalidParameters, condition, "only configmaps can be used as parameterReferences")
		}
		if paramRef.Name == "" || paramRef.Namespace == nil {
			return r.setClassCondition(ctx, req, gatewayapis.GatewayClassReasonInvalidParameters, condition, "paramReference name and namespace are required")
		}
		var parameters v1.ConfigMap
		key := types.NamespacedName{
			Namespace: string(*paramRef.Namespace),
			Name:      paramRef.Name,
		}
		if err := r.Client.Get(ctx, key, &parameters); err != nil {
			return r.setClassCondition(ctx, req, gatewayapis.GatewayClassReasonInvalidParameters, condition, err.Error())
		}
		logger.Info("got parameters reference values", "values", parameters.Data)
	}

	condition = metav1.ConditionTrue
	return r.setClassCondition(ctx, req, gatewayapis.GatewayClassReasonAccepted, condition, "GatewayClass is accepted")
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayapis.GatewayClass{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return r.isManagedClass(e.Object)
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				// We don't want to trigger updates only when finalizers are being added or removed
				return (len(e.ObjectOld.GetFinalizers()) == len(e.ObjectNew.GetFinalizers()) &&
					r.isManagedClass(e.ObjectNew))
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				// The reconciler adds a finalizer so we perform clean-up
				// when the delete timestamp is added
				// Suppress Delete events to avoid filtering them out in the Reconcile function
				return false
			},
		}).
		Complete(r)
}

// isManagedClass is used by predicate functions to define if this is an object managed by this controller
func (r *GatewayClassReconciler) isManagedClass(obj client.Object) bool {
	class, ok := obj.(*gatewayapis.GatewayClass)
	if !ok {
		return false
	}
	return class.Spec.ControllerName == gatewayapis.GatewayController(r.ControllerName)
}

// setClassCondition is a function to set (and retry) the conditions of a gatewayclass
func (r *GatewayClassReconciler) setClassCondition(ctx context.Context, req ctrl.Request, conditionReason gatewayapis.GatewayClassConditionReason, accepted metav1.ConditionStatus, msg string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.WithValues("controller", req.Name)
	if accepted != metav1.ConditionStatus(gatewayapis.GatewayClassReasonAccepted) {
		logger.Error(fmt.Errorf("%s", msg), "error reconciling class")
	}
	condition := metav1.Condition{
		Type:    string(gatewayapis.GatewayClassConditionStatusAccepted),
		Status:  accepted,
		Reason:  string(conditionReason),
		Message: msg,
	}

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var updatedClass gatewayapis.GatewayClass
		if err := r.Get(ctx, req.NamespacedName, &updatedClass); err != nil {
			return err
		}

		if updatedClass.Status.Conditions == nil {
			updatedClass.Status.Conditions = make([]metav1.Condition, 0)
		}
		meta.SetStatusCondition(&updatedClass.Status.Conditions, condition)
		return r.Status().Update(ctx, &updatedClass)
	})
	return ctrl.Result{}, retryErr
}
