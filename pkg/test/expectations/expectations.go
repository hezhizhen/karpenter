/*
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

package expectations

import (
	"context"
	"fmt"
	"sync"
	"time"

	//nolint:revive,stylecheck
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/controllers/provisioning"
	"github.com/aws/karpenter/pkg/controllers/selection"
)

const (
	ReconcilerPropagationTime = 10 * time.Second
	RequestInterval           = 1 * time.Second
)

func ExpectPodExists(ctx context.Context, c client.Client, name string, namespace string) *v1.Pod {
	pod := &v1.Pod{}
	Expect(c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, pod)).To(Succeed())
	return pod
}

func ExpectNodeExists(ctx context.Context, c client.Client, name string) *v1.Node {
	node := &v1.Node{}
	Expect(c.Get(ctx, client.ObjectKey{Name: name}, node)).To(Succeed())
	return node
}

func ExpectNotFound(ctx context.Context, c client.Client, objects ...client.Object) {
	for _, object := range objects {
		Eventually(func() bool {
			return errors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: object.GetName(), Namespace: object.GetNamespace()}, object))
		}, ReconcilerPropagationTime, RequestInterval).Should(BeTrue(), func() string {
			return fmt.Sprintf("expected %s to be deleted, but it still exists", object.GetSelfLink())
		})
	}
}

func ExpectScheduled(ctx context.Context, c client.Client, pod *v1.Pod) *v1.Node {
	p := ExpectPodExists(ctx, c, pod.Name, pod.Namespace)
	Expect(p.Spec.NodeName).ToNot(BeEmpty(), fmt.Sprintf("expected %s/%s to be scheduled", pod.Namespace, pod.Name))
	return ExpectNodeExists(ctx, c, p.Spec.NodeName)
}

func ExpectNotScheduled(ctx context.Context, c client.Client, pod *v1.Pod) {
	p := ExpectPodExists(ctx, c, pod.Name, pod.Namespace)
	Eventually(p.Spec.NodeName).Should(BeEmpty(), fmt.Sprintf("expected %s/%s to not be scheduled", pod.Namespace, pod.Name))
}

func ExpectApplied(ctx context.Context, c client.Client, objects ...client.Object) {
	for _, object := range objects {
		if object.GetResourceVersion() == "" {
			Expect(c.Create(ctx, object)).To(Succeed())
		} else {
			Expect(c.Update(ctx, object)).To(Succeed())
		}
	}
}

func ExpectStatusUpdated(ctx context.Context, c client.Client, objects ...client.Object) {
	for _, object := range objects {
		Expect(c.Status().Update(ctx, object)).To(Succeed())
	}
}

func ExpectCreated(ctx context.Context, c client.Client, objects ...client.Object) {
	for _, object := range objects {
		Expect(c.Create(ctx, object)).To(Succeed())
	}
}

func ExpectCreatedWithStatus(ctx context.Context, c client.Client, objects ...client.Object) {
	for _, object := range objects {
		updatecopy := object.DeepCopyObject().(client.Object)
		deletecopy := object.DeepCopyObject().(client.Object)
		ExpectApplied(ctx, c, object)
		Expect(c.Status().Update(ctx, updatecopy)).To(Succeed())
		if deletecopy.GetDeletionTimestamp() != nil {
			Expect(c.Delete(ctx, deletecopy, &client.DeleteOptions{GracePeriodSeconds: ptr.Int64(int64(time.Until(deletecopy.GetDeletionTimestamp().Time).Seconds()))})).ToNot(HaveOccurred())
		}
	}
}

func ExpectDeleted(ctx context.Context, c client.Client, objects ...client.Object) {
	for _, object := range objects {
		if err := c.Delete(ctx, object, &client.DeleteOptions{GracePeriodSeconds: ptr.Int64(0)}); !errors.IsNotFound(err) {
			Expect(err).To(BeNil())
		}
		ExpectNotFound(ctx, c, object)
	}
}

func ExpectCleanedUp(ctx context.Context, c client.Client) {
	wg := sync.WaitGroup{}
	namespaces := &v1.NamespaceList{}
	Expect(c.List(ctx, namespaces)).To(Succeed())
	nodes := &v1.NodeList{}
	Expect(c.List(ctx, nodes))
	for i := range nodes.Items {
		nodes.Items[i].SetFinalizers([]string{})
		Expect(c.Update(ctx, &nodes.Items[i])).To(Succeed())
	}
	for _, object := range []client.Object{
		&v1.Pod{},
		&v1.Node{},
		&appsv1.DaemonSet{},
		&v1beta1.PodDisruptionBudget{},
		&v1.PersistentVolumeClaim{},
		&v1alpha5.Provisioner{},
	} {
		for _, namespace := range namespaces.Items {
			wg.Add(1)
			go func(object client.Object, namespace string) {
				Expect(c.DeleteAllOf(ctx, object, client.InNamespace(namespace))).ToNot(HaveOccurred())
				wg.Done()
			}(object, namespace.Name)
		}
	}
	wg.Wait()
}

// ExpectProvisioningCleanedUp includes additional cleanup logic for provisioning workflows
func ExpectProvisioningCleanedUp(ctx context.Context, c client.Client, controller *provisioning.Controller) {
	provisioners := v1alpha5.ProvisionerList{}
	Expect(c.List(ctx, &provisioners)).To(Succeed())
	ExpectCleanedUp(ctx, c)
	for i := range provisioners.Items {
		ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(&provisioners.Items[i]))
	}
}

func ExpectProvisioned(ctx context.Context, c client.Client, selectionController *selection.Controller, provisioningController *provisioning.Controller, provisioner *v1alpha5.Provisioner, pods ...*v1.Pod) (result []*v1.Pod) {
	provisioning.MaxPodsPerBatch = len(pods)
	// Persist objects
	ExpectApplied(ctx, c, provisioner)
	ExpectStatusUpdated(ctx, c, provisioner)
	for _, pod := range pods {
		ExpectCreatedWithStatus(ctx, c, pod)
	}
	// Wait for reconcile
	ExpectReconcileSucceeded(ctx, provisioningController, client.ObjectKeyFromObject(provisioner))
	wg := sync.WaitGroup{}
	for _, pod := range pods {
		wg.Add(1)
		go func(pod *v1.Pod) {
			selectionController.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(pod)})
			wg.Done()
		}(pod)
	}
	wg.Wait()
	// Return updated pods
	for _, pod := range pods {
		result = append(result, ExpectPodExists(ctx, c, pod.GetName(), pod.GetNamespace()))
	}
	return result
}

func ExpectReconcileSucceeded(ctx context.Context, reconciler reconcile.Reconciler, key client.ObjectKey) {
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	Expect(err).ToNot(HaveOccurred())
}
