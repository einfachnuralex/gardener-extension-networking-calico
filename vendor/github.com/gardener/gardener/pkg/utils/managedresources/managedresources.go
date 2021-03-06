// Copyright (c) 2020 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package managedresources

import (
	"context"
	"fmt"
	"time"

	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	"github.com/gardener/gardener/pkg/chartrenderer"
	"github.com/gardener/gardener/pkg/utils/chart"
	"github.com/gardener/gardener/pkg/utils/imagevector"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/gardener/gardener/pkg/utils/kubernetes/health"
	"github.com/gardener/gardener/pkg/utils/retry"

	resourcesv1alpha1 "github.com/gardener/gardener-resource-manager/pkg/apis/resources/v1alpha1"
	"github.com/gardener/gardener-resource-manager/pkg/manager"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sretry "k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// SecretPrefix is the prefix that can be used for secrets referenced by managed resources.
	SecretPrefix = "managedresource-"
	// LabelKeyOrigin is a key for a label on a managed resource with the value 'origin'.
	LabelKeyOrigin = "origin"
	// LabelValueGardener is a value for a label on a managed resource with the value 'gardener'.
	LabelValueGardener = "gardener"
)

// SecretName returns the name of a corev1.Secret for the given name of a resourcesv1alpha1.ManagedResource. If
// <withPrefix> is set then the name will be prefixed with 'managedresource-'.
func SecretName(name string, withPrefix bool) string {
	if withPrefix {
		return SecretPrefix + name
	}
	return name
}

// New initiates a new ManagedResource object which can be reconciled.
func New(client client.Client, namespace, name, class string, keepObjects *bool, labels, injectedLabels map[string]string, forceOverwriteAnnotations *bool) *manager.ManagedResource {
	mr := manager.
		NewManagedResource(client).
		WithNamespacedName(namespace, name).
		WithClass(class).
		WithLabels(labels).
		WithInjectedLabels(injectedLabels)

	if keepObjects != nil {
		mr = mr.KeepObjects(*keepObjects)
	}
	if forceOverwriteAnnotations != nil {
		mr = mr.ForceOverwriteAnnotations(*forceOverwriteAnnotations)
	}

	return mr
}

// NewForShoot constructs a new ManagedResource object for the shoot's Gardener-Resource-Manager.
func NewForShoot(c client.Client, namespace, name string, keepObjects bool) *manager.ManagedResource {
	var (
		injectedLabels = map[string]string{v1beta1constants.ShootNoCleanup: "true"}
		labels         = map[string]string{LabelKeyOrigin: LabelValueGardener}
	)

	return New(c, namespace, name, "", &keepObjects, labels, injectedLabels, nil)
}

// NewForSeed constructs a new ManagedResource object for the seed's Gardener-Resource-Manager.
func NewForSeed(c client.Client, namespace, name string, keepObjects bool) *manager.ManagedResource {
	return New(c, namespace, name, v1beta1constants.SeedResourceManagerClass, &keepObjects, nil, nil, nil)
}

// NewSecret initiates a new Secret object which can be reconciled.
func NewSecret(client client.Client, namespace, name string, data map[string][]byte, secretNameWithPrefix bool) (string, *manager.Secret) {
	secretName := SecretName(name, secretNameWithPrefix)
	return secretName, manager.
		NewSecret(client).
		WithNamespacedName(namespace, secretName).
		WithKeyValues(data)
}

// CreateFromUnstructured creates a managed resource and its secret with the given name, class, and objects in the given namespace.
func CreateFromUnstructured(ctx context.Context, client client.Client, namespace, name string, secretNameWithPrefix bool, class string, objs []*unstructured.Unstructured, keepObjects bool, injectedLabels map[string]string) error {
	var data []byte
	for _, obj := range objs {
		bytes, err := obj.MarshalJSON()
		if err != nil {
			return errors.Wrapf(err, "marshal failed for '%s/%s' for secret '%s/%s'", obj.GetNamespace(), obj.GetName(), namespace, name)
		}
		data = append(data, []byte("\n---\n")...)
		data = append(data, bytes...)
	}
	return Create(ctx, client, namespace, name, secretNameWithPrefix, class, map[string][]byte{name: data}, &keepObjects, injectedLabels, pointer.BoolPtr(false))
}

// Create creates a managed resource and its secret with the given name, class, key, and data in the given namespace.
func Create(ctx context.Context, client client.Client, namespace, name string, secretNameWithPrefix bool, class string, data map[string][]byte, keepObjects *bool, injectedLabels map[string]string, forceOverwriteAnnotations *bool) error {
	var (
		secretName, secret = NewSecret(client, namespace, name, data, secretNameWithPrefix)
		managedResource    = New(client, namespace, name, class, keepObjects, nil, injectedLabels, forceOverwriteAnnotations).WithSecretRef(secretName)
	)

	return deployManagedResource(ctx, secret, managedResource)
}

// CreateForSeed deploys a ManagedResource CR for the seed's gardener-resource-manager.
func CreateForSeed(ctx context.Context, client client.Client, namespace, name string, keepObjects bool, data map[string][]byte) error {
	var (
		secretName, secret = NewSecret(client, namespace, name, data, true)
		managedResource    = NewForSeed(client, namespace, name, keepObjects).WithSecretRef(secretName)
	)

	return deployManagedResource(ctx, secret, managedResource)
}

// CreateForShoot deploys a ManagedResource CR for the shoot's gardener-resource-manager.
func CreateForShoot(ctx context.Context, client client.Client, namespace, name string, keepObjects bool, data map[string][]byte) error {
	var (
		secretName, secret = NewSecret(client, namespace, name, data, true)
		managedResource    = NewForShoot(client, namespace, name, keepObjects).WithSecretRef(secretName)
	)

	return deployManagedResource(ctx, secret, managedResource)
}

func deployManagedResource(ctx context.Context, secret *manager.Secret, managedResource *manager.ManagedResource) error {
	if err := secret.Reconcile(ctx); err != nil {
		return errors.Wrapf(err, "could not create or update secret of managed resources")
	}

	if err := managedResource.Reconcile(ctx); err != nil {
		return errors.Wrapf(err, "could not create or update managed resource")
	}

	return nil
}

// Delete deletes the managed resource and its secret with the given name in the given namespace.
func Delete(ctx context.Context, client client.Client, namespace string, name string, secretNameWithPrefix bool) error {
	secretName := SecretName(name, secretNameWithPrefix)

	if err := manager.
		NewManagedResource(client).
		WithNamespacedName(namespace, name).
		Delete(ctx); err != nil {
		return errors.Wrapf(err, "could not delete managed resource '%s/%s'", namespace, name)
	}

	if err := manager.
		NewSecret(client).
		WithNamespacedName(namespace, secretName).
		Delete(ctx); err != nil {
		return errors.Wrapf(err, "could not delete secret '%s/%s' of managed resource", namespace, secretName)
	}

	return nil
}

var (
	// DeleteForSeed is a function alias for deleteWithSecretNamePrefix.
	DeleteForSeed = deleteWithSecretNamePrefix
	// DeleteForShoot is a function alias for deleteWithSecretNamePrefix.
	DeleteForShoot = deleteWithSecretNamePrefix
)

func deleteWithSecretNamePrefix(ctx context.Context, client client.Client, namespace string, name string) error {
	return Delete(ctx, client, namespace, name, true)
}

// IntervalWait is the interval when waiting for managed resources.
var IntervalWait = 2 * time.Second

// WaitUntilHealthy waits until the given managed resource is healthy.
func WaitUntilHealthy(ctx context.Context, client client.Client, namespace, name string) error {
	obj := &resourcesv1alpha1.ManagedResource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}

	return retry.Until(ctx, IntervalWait, func(ctx context.Context) (done bool, err error) {
		if err := client.Get(ctx, kutil.Key(namespace, name), obj); err != nil {
			return retry.SevereError(err)
		}

		if err := health.CheckManagedResource(obj); err != nil {
			return retry.MinorError(fmt.Errorf("managed resource %s/%s is not healthy", namespace, name))
		}

		return retry.Ok()
	})
}

// WaitUntilDeleted waits until the given managed resource is deleted.
func WaitUntilDeleted(ctx context.Context, client client.Client, namespace, name string) error {
	mr := &resourcesv1alpha1.ManagedResource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return kutil.WaitUntilResourceDeleted(ctx, client, mr, IntervalWait)
}

// SetKeepObjects updates the keepObjects field of the managed resource with the given name in the given namespace.
func SetKeepObjects(ctx context.Context, c client.Client, namespace, name string, keepObjects bool) error {
	resource := &resourcesv1alpha1.ManagedResource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}

	if err := kutil.TryUpdate(ctx, k8sretry.DefaultBackoff, c, resource, func() error {
		resource.Spec.KeepObjects = &keepObjects
		return nil
	}); client.IgnoreNotFound(err) != nil {
		return errors.Wrapf(err, "could not update managed resource '%s/%s'", namespace, name)
	}

	return nil
}

// RenderChartAndCreate renders a chart and creates a ManagedResource for the gardener-resource-manager
// out of the results.
func RenderChartAndCreate(ctx context.Context, namespace string, name string, secretNameWithPrefix bool, client client.Client, chartRenderer chartrenderer.Interface, chart chart.Interface, values map[string]interface{}, imageVector imagevector.ImageVector, chartNamespace string, version string, withNoCleanupLabel bool, forceOverwriteAnnotations bool) error {
	chartName, data, err := chart.Render(chartRenderer, chartNamespace, imageVector, version, version, values)
	if err != nil {
		return errors.Wrapf(err, "could not render chart")
	}

	// Create or update managed resource referencing the previously created secret
	var injectedLabels map[string]string
	if withNoCleanupLabel {
		injectedLabels = map[string]string{v1beta1constants.ShootNoCleanup: "true"}
	}

	return Create(ctx, client, namespace, name, secretNameWithPrefix, "", map[string][]byte{chartName: data}, pointer.BoolPtr(false), injectedLabels, &forceOverwriteAnnotations)
}
