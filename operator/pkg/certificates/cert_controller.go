// Copyright (c) Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project
// Licensed under the Apache License 2.0

package certificates

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"reflect"
	"time"

	"golang.org/x/exp/slices"
	appv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stolostron/multicluster-global-hub/operator/api/operator/v1alpha4"
	"github.com/stolostron/multicluster-global-hub/operator/pkg/config"
	"github.com/stolostron/multicluster-global-hub/pkg/constants"
	"github.com/stolostron/multicluster-global-hub/pkg/utils"
)

const (
	restartLabel = "cert/time-restarted"
)

var (
	caSecretNames            = []string{serverCACerts, clientCACerts}
	isCertControllerRunnning = false
)

func Start(ctx context.Context, c client.Client, kubeClient kubernetes.Interface) {
	if isCertControllerRunnning {
		return
	}
	isCertControllerRunnning = true

	watchlist := cache.NewListWatchFromClient(
		kubeClient.CoreV1().RESTClient(),
		"secrets",
		utils.GetDefaultNamespace(),
		fields.OneTermEqualSelector("metadata.namespace", utils.GetDefaultNamespace()),
	)
	options := cache.InformerOptions{
		ListerWatcher: watchlist,
		ObjectType:    &v1.Secret{},
		ResyncPeriod:  time.Minute * 60,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    onAdd(c),
			DeleteFunc: onDelete(c),
			UpdateFunc: onUpdate(ctx, c),
		},
	}
	_, controller := cache.NewInformerWithOptions(options)

	go controller.Run(ctx.Done())
}

func updateDeployLabel(c client.Client, isUpdate bool) {
	dep := &appv1.Deployment{}
	err := c.Get(context.TODO(), types.NamespacedName{
		Name:      constants.InventoryDeploymentName,
		Namespace: utils.GetDefaultNamespace(),
	}, dep)
	if err != nil {
		if !errors.IsNotFound(err) {
			log.Error(err, "Failed to check the deployment", "name", constants.InventoryDeploymentName)
		}
		return
	}
	if isUpdate || dep.Status.ReadyReplicas != 0 {
		newDep := dep.DeepCopy()
		newDep.Spec.Template.ObjectMeta.Labels[restartLabel] = time.Now().Format("2006-1-2.1504")
		err := c.Patch(context.TODO(), newDep, client.StrategicMergeFrom(dep))
		if err != nil {
			log.Error(err, "Failed to update the deployment", "name", constants.InventoryDeploymentName)
		} else {
			log.Info("Update deployment cert/restart label", "name", constants.InventoryDeploymentName)
		}
	}
}

func needsRenew(s v1.Secret) bool {
	certSecretNames := []string{serverCACerts, clientCACerts, serverCerts, guestCerts}
	if !slices.Contains(certSecretNames, s.Name) {
		return false
	}
	data := s.Data[tlsCertName]
	if len(data) == 0 {
		log.Info("miss cert, need to recreate", "name", s.Name)
		return true
	}
	block, _ := pem.Decode(data)
	certs, err := x509.ParseCertificates(block.Bytes)
	if err != nil {
		log.Error(err, "wrong certificate found, need to recreate", "name", s.Name)
		return true
	}
	cert := certs[0]
	maxWait := cert.NotAfter.Sub(cert.NotBefore) / 5
	latestTime := cert.NotAfter.Add(-maxWait)
	if time.Now().After(latestTime) {
		log.Info(fmt.Sprintf("certificate expired in %6.3f hours, need to renew",
			time.Until(cert.NotAfter).Hours()), "secret", s.Name)
		return true
	}

	return false
}

func onAdd(c client.Client) func(obj interface{}) {
	return func(obj interface{}) {
		updateDeployLabel(c, false)
	}
}

func onDelete(c client.Client) func(obj interface{}) {
	return func(obj interface{}) {
		s := *obj.(*v1.Secret)
		if !slices.Contains(caSecretNames, s.Name) {
			return
		}
		mgh := &v1alpha4.MulticlusterGlobalHub{}
		err := c.Get(context.TODO(), config.GetMGHNamespacedName(), mgh)
		if err != nil {
			return
		}
		log.Info(
			"secret for ca certificate deleted by mistake, add the cert back to the new created one",
			"name",
			s.Name,
		)
		i := 0
		for {
			caSecret := &v1.Secret{}
			err = c.Get(context.TODO(), types.NamespacedName{
				Name:      s.Name,
				Namespace: utils.GetDefaultNamespace(),
			}, caSecret)
			if err == nil {
				caSecret.Data[tlsCertName] = append(caSecret.Data[tlsCertName], s.Data[tlsCertName]...)
				err = c.Update(context.TODO(), caSecret)
				if err != nil {
					log.Error(err, "Failed to update secret for ca certificate", "name", s.Name)
					i++
				} else {
					break
				}
			} else {
				// wait mgh operator recreate the ca certificate at most 30 seconds
				if i < 6 {
					time.Sleep(5 * time.Second)
					i++
				} else {
					log.Info("new secret for ca certificate not created")
					break
				}
			}
		}
	}
}

func onUpdate(ctx context.Context, c client.Client) func(oldObj, newObj interface{}) {
	return func(oldObj, newObj interface{}) {
		oldS := *oldObj.(*v1.Secret)
		newS := *newObj.(*v1.Secret)
		if !reflect.DeepEqual(oldS.Data, newS.Data) {
			updateDeployLabel(c, true)
		} else {
			if slices.Contains(caSecretNames, newS.Name) {
				removeExpiredCA(c, newS.Name, newS.Namespace)
			}
			if needsRenew(newS) {
				var err error
				var hosts []string
				switch name := newS.Name; {
				case name == serverCACerts:
					err, _ = createCASecret(c, nil, nil, true, serverCACerts, newS.Namespace, serverCACertificateCN)
				case name == clientCACerts:
					err, _ = createCASecret(c, nil, nil, true, clientCACerts, newS.Namespace, clientCACertificateCN)
				case name == serverCerts:
					hosts, err = getHosts(ctx, c, newS.Namespace)
					if err == nil {
						err = createCertSecret(c, nil, nil, true, serverCerts, newS.Namespace, true, serverCertificateCN, nil, hosts, nil)
					}
				default:
					return
				}
				if err != nil {
					log.Error(err, "Failed to renew the certificate", "name", newS.Name)
				}
			}
		}
	}
}