package generic

import (
	"context"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	policiesv1 "open-cluster-management.io/governance-policy-propagator/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/stolostron/multicluster-global-hub/pkg/constants"
)

func TestAddRemoveFinalizer(t *testing.T) {
	namespacedName := types.NamespacedName{
		Name:      "test",
		Namespace: "default",
	}
	policy := &policiesv1.Policy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      namespacedName.Name,
			Namespace: namespacedName.Namespace,
		},
		Spec: policiesv1.PolicySpec{},
	}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypes(policiesv1.GroupVersion, policy)
	c := fake.NewFakeClientWithScheme(scheme)

	controller := &genericStatusSyncController{
		client:        c,
		log:           ctrl.Log.WithName("test-controller"),
		finalizerName: constants.GlobalHubCleanupFinalizer,
		lock:          sync.Mutex{},
	}

	if err := controller.removeFinalizer(context.TODO(), policy, controller.log); err != nil {
		t.Fatal(err)
	}

	if err := controller.updateObjectAndFinalizer(context.TODO(), policy, controller.log); err == nil {
		t.Fatal("Expect to report error")
	}

	//create the object
	if err := c.Create(context.TODO(), policy, &client.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := controller.updateObjectAndFinalizer(context.TODO(), policy, controller.log); err != nil {
		t.Fatal(err)
	}

	//do nothing
	if err := controller.addFinalizer(context.TODO(), policy, controller.log); err != nil {
		t.Fatal(err)
	}

	runtimePolicy := &policiesv1.Policy{}
	if err := c.Get(context.TODO(), namespacedName, runtimePolicy); err != nil {
		t.Fatal(err)
	}

	if !controllerutil.ContainsFinalizer(runtimePolicy, constants.GlobalHubCleanupFinalizer) {
		t.Fatalf("Expect to have the finalizer %s", constants.GlobalHubCleanupFinalizer)
	}

	if err := controller.deleteObjectAndFinalizer(context.TODO(), policy, controller.log); err != nil {
		t.Fatal(err)
	}

	//do nothing
	if err := controller.removeFinalizer(context.TODO(), policy, controller.log); err != nil {
		t.Fatal(err)
	}

	runtimePolicy = &policiesv1.Policy{}
	if err := c.Get(context.TODO(), namespacedName, runtimePolicy); err != nil {
		t.Fatal(err)
	}

	if controllerutil.ContainsFinalizer(runtimePolicy, constants.GlobalHubCleanupFinalizer) {
		t.Fatalf("Expect no finalizer %s", constants.GlobalHubCleanupFinalizer)
	}

	if err := c.Delete(context.TODO(), policy, &client.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	controllerutil.AddFinalizer(policy, constants.GlobalHubCleanupFinalizer)
	if err := controller.removeFinalizer(context.TODO(), policy, controller.log); err == nil {
		t.Fatal("Expect to report error")
	}
}