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

package backup

import (
	"context"
	"database/sql"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/stolostron/multicluster-global-hub/pkg/constants"
	"github.com/stolostron/multicluster-global-hub/pkg/database"
	"github.com/stolostron/multicluster-global-hub/pkg/utils"
)

var (
	backupLog = ctrl.Log.WithName("backup pvc")
)

// BackupReconciler reconciles a MulticlusterGlobalHub object
type BackupPVCReconciler struct {
	manager.Manager
	client.Client
	sqlConn *sql.Conn
}

func NewBackupPVCReconciler(mgr manager.Manager, sqlConn *sql.Conn) *BackupPVCReconciler {
	return &BackupPVCReconciler{
		Manager: mgr,
		Client:  mgr.GetClient(),
		sqlConn: sqlConn,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackupPVCReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("backupPvcController").
		For(&corev1.PersistentVolumeClaim{},
			builder.WithPredicates(pvcPred)).
		Complete(r)
}

var pvcPred = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool {
		return IfPvcNeedBackup(e.Object)
	},
	UpdateFunc: func(e event.UpdateEvent) bool {
		return IfPvcNeedBackup(e.ObjectNew)
	},
	DeleteFunc: func(e event.DeleteEvent) bool {
		return false
	},
}

func IfPvcNeedBackup(pvc client.Object) bool {
	//Only watch pvcs which need backup
	if !utils.HasLabel(pvc.GetLabels(), constants.BackupVolumnKey, constants.BackupGlobalHubValue) {
		return false
	}
	//Only run backup when volsync is waiting for trigger
	if !utils.HasLabel(pvc.GetAnnotations(), constants.BackupPvcLatestCopyStatus, constants.BackupPvcWaitingForTrigger) {
		return false
	}
	//If volsync is in progress, just wait volsync finish
	if utils.HasLabelKey(pvc.GetAnnotations(), constants.BackupPvcLatestCopyTrigger) {
		//Wait volsync process
		if pvc.GetAnnotations()[constants.BackupPvcLatestCopyTrigger] !=
			pvc.GetAnnotations()[constants.BackupPvcCopyTrigger] {
			return false
		}
	}
	return true
}

func (r *BackupPVCReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	isBackupEnabled, err := utils.IsBackupEnabled(ctx, r.Client)
	if err != nil {
		backupLog.Error(err, "failed to get backup enabled", "req", req)
		return ctrl.Result{}, err
	}
	database.IsBackupEnabled = isBackupEnabled
	if !isBackupEnabled {
		backupLog.V(2).Info("Backup is not enabled")
		return ctrl.Result{}, nil
	}

	backupLog.V(2).Info("Start backup pvc", "req", req)

	err = database.Lock(r.sqlConn)
	if err != nil {
		backupLog.Error(err, "failed to get db lock")
		return ctrl.Result{}, err
	}

	defer database.Unlock(r.sqlConn)

	triggerTime := time.Now().Format(time.RFC3339)
	formatTriggerTime := strings.ReplaceAll(triggerTime, ":", ".")
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		pvc := &corev1.PersistentVolumeClaim{}
		err = r.Client.Get(ctx, req.NamespacedName, pvc)
		if err != nil {
			return err
		}
		updatePvc := pvc.DeepCopy()
		utils.AddAnnotations(updatePvc, map[string]string{
			constants.BackupPvcCopyTrigger: formatTriggerTime,
		})
		if err := r.Client.Update(ctx, updatePvc); err != nil {
			klog.Errorf("Failed to update pvc %v, err:%v", req.NamespacedName, err)
			return err
		}
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	backupLog.V(2).Info("Start wait pvc backup finish", "time", time.Now())
	if err := wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
		pvc := &corev1.PersistentVolumeClaim{}
		err := r.Client.Get(ctx, req.NamespacedName, pvc)
		if err != nil {
			return false, nil
		}
		if !utils.HasLabel(pvc.GetAnnotations(), constants.BackupPvcLatestCopyStatus, constants.BackupPvcCompletedTrigger) {
			return false, nil
		}
		if !utils.HasLabel(pvc.GetAnnotations(), constants.BackupPvcLatestCopyTrigger, formatTriggerTime) {
			return false, nil
		}
		return true, nil
	}); err != nil {
		backupLog.Error(err, "Time out to wait backup pvc finished")
		return ctrl.Result{}, err
	}
	backupLog.V(2).Info("pvc backup finish", "time", time.Now())
	return ctrl.Result{}, nil
}