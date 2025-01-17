package syncers

import (
	"context"
	"encoding/json"
	"time"

	"github.com/go-logr/logr"
	klusterletv1alpha1 "github.com/stolostron/cluster-lifecycle-api/klusterletconfig/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	operatorv1 "open-cluster-management.io/api/operator/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bundleevent "github.com/stolostron/multicluster-global-hub/pkg/bundle/event"
	"github.com/stolostron/multicluster-global-hub/pkg/constants"
)

const (
	bootstrapSecretBackupSuffix = "-backup"
)

type managedClusterMigrationFromSyncer struct {
	log    logr.Logger
	client client.Client
}

func NewManagedClusterMigrationFromSyncer(client client.Client) *managedClusterMigrationFromSyncer {
	return &managedClusterMigrationFromSyncer{
		log:    ctrl.Log.WithName("managed-cluster-migration-from-syncer"),
		client: client,
	}
}

func (s *managedClusterMigrationFromSyncer) Sync(ctx context.Context, payload []byte) error {
	// handle migration.from cloud event
	managedClusterMigrationEvent := &bundleevent.ManagedClusterMigrationFromEvent{}
	if err := json.Unmarshal(payload, managedClusterMigrationEvent); err != nil {
		return err
	}

	// create or update bootstrap secret
	bootstrapSecret := managedClusterMigrationEvent.BootstrapSecret
	foundBootstrapSecret := &corev1.Secret{}
	if err := s.client.Get(ctx,
		types.NamespacedName{
			Name:      bootstrapSecret.Name,
			Namespace: bootstrapSecret.Namespace,
		}, foundBootstrapSecret); err != nil {
		if apierrors.IsNotFound(err) {
			s.log.Info("creating bootstrap secret", "bootstrap secret", bootstrapSecret)
			if err := s.client.Create(ctx, bootstrapSecret); err != nil {
				return err
			}
		} else {
			return err
		}
	} else {
		// update the bootstrap secret if it already exists
		s.log.Info("updating bootstrap secret", "bootstrap secret", bootstrapSecret)
		if err := s.client.Update(ctx, bootstrapSecret); err != nil {
			return err
		}
	}

	// create or update boostrap secret backup
	bootstrapSecretBackup := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bootstrapSecret.Name + bootstrapSecretBackupSuffix,
			Namespace: bootstrapSecret.Namespace,
		},
		Data: bootstrapSecret.Data,
	}
	foundBootstrapSecretBackup := &corev1.Secret{}
	if err := s.client.Get(ctx,
		types.NamespacedName{
			Name:      bootstrapSecretBackup.Name,
			Namespace: bootstrapSecretBackup.Namespace,
		}, foundBootstrapSecretBackup); err != nil {
		if apierrors.IsNotFound(err) {
			s.log.Info("creating bootstrap backup secret", "bootstrap backup secret", bootstrapSecretBackup)
			if err := s.client.Create(ctx, bootstrapSecretBackup); err != nil {
				return err
			}
		} else {
			return err
		}
	} else {
		// update the bootstrap backup secret if it already exists
		s.log.Info("updating bootstrap backup secret", "bootstrap backup secret", bootstrapSecretBackup)
		if err := s.client.Update(ctx, bootstrapSecretBackup); err != nil {
			return err
		}
	}

	// create klusterlet config if it does not exist
	klusterletConfig := managedClusterMigrationEvent.KlusterletConfig
	// set the bootstrap kubeconfig secrets in klusterlet config
	klusterletConfig.Spec.BootstrapKubeConfigs.LocalSecrets.KubeConfigSecrets = []operatorv1.KubeConfigSecret{
		{
			Name: bootstrapSecret.Name,
		},
		{
			Name: bootstrapSecretBackup.Name,
		},
	}
	foundKlusterletConfig := &klusterletv1alpha1.KlusterletConfig{}
	if err := s.client.Get(ctx,
		types.NamespacedName{
			Name: klusterletConfig.Name,
		}, foundKlusterletConfig); err != nil {
		if apierrors.IsNotFound(err) {
			s.log.Info("creating klusterlet config", "klusterlet config", klusterletConfig)
			if err := s.client.Create(ctx, klusterletConfig); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	// update managed cluster annotations to point to the new klusterlet config
	managedClusters := managedClusterMigrationEvent.ManagedClusters
	for _, managedCluster := range managedClusters {
		mcl := &clusterv1.ManagedCluster{}
		if err := s.client.Get(ctx, types.NamespacedName{
			Name: managedCluster,
		}, mcl); err != nil {
			return err
		}
		annotations := mcl.Annotations
		if annotations == nil {
			annotations = make(map[string]string)
		}

		_, migrating := annotations[constants.ManagedClusterMigrating]
		if migrating && annotations["agent.open-cluster-management.io/klusterlet-config"] == klusterletConfig.Name {
			continue
		}
		annotations["agent.open-cluster-management.io/klusterlet-config"] = klusterletConfig.Name
		annotations[constants.ManagedClusterMigrating] = ""
		mcl.SetAnnotations(annotations)
		if err := s.client.Update(ctx, mcl); err != nil {
			return err
		}
	}

	// check managed cluster available unknown status and detach the managed cluster in new go routine
	if err := s.detachManagedClusters(ctx, managedClusters); err != nil {
		s.log.Error(err, "failed to detach managed clusters")
	}

	return nil
}

func (s *managedClusterMigrationFromSyncer) detachManagedClusters(ctx context.Context, managedClusters []string) error {
	return wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		for _, managedCluster := range managedClusters {
			mcl := &clusterv1.ManagedCluster{}
			if err := s.client.Get(ctx, types.NamespacedName{
				Name: managedCluster,
			}, mcl); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				} else {
					return false, err
				}
			}
			if meta.IsStatusConditionPresentAndEqual(mcl.Status.Conditions,
				clusterv1.ManagedClusterConditionAvailable, metav1.ConditionUnknown) {
				if err := s.client.Delete(ctx, mcl); err != nil {
					return false, err
				}
			} else {
				return false, nil
			}
		}
		return true, nil
	})
}
