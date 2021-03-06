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

package controllers

import (
	"context"
	"fmt"

	"github.com/jenkinsci/jenkins-automation-operator/pkg/notifications/event"
	"github.com/jenkinsci/jenkins-automation-operator/pkg/notifications/reason"

	"github.com/operator-framework/operator-lib/status"

	"github.com/jenkinsci/jenkins-automation-operator/pkg/exec"

	"github.com/jenkinsci/jenkins-automation-operator/pkg/configuration/base/resources"
	"github.com/jenkinsci/jenkins-automation-operator/pkg/log"

	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stackerr "github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha2 "github.com/jenkinsci/jenkins-automation-operator/api/v1alpha2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// RestoreReconciler reconciles a Restore object
type RestoreReconciler struct {
	client.Client
	Log                logr.Logger
	Scheme             *runtime.Scheme
	NotificationEvents chan event.Event
}

var (
	restoreLogger = log.Log.WithName("restore")
	// RestoreInitialized and other Condition Types
	RestoreInitialized status.ConditionType = "RestoreInitialized"
	RestoreCompleted   status.ConditionType = "RestoreCompleted"
	RestartStarted     status.ConditionType = "RestartStarted"
	SafeRestartStarted status.ConditionType = "SafeRestartStarted"
)

// +kubebuilder:rbac:groups=jenkins.io,resources=restores;restores/status,verbs=*

func (r *RestoreReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	restoreLogger := r.Log.WithValues("restore", req.NamespacedName)
	execClient := exec.NewKubeExecClient()

	// Fetch the Restore instance
	restoreInstance := &v1alpha2.Restore{}
	err := r.Client.Get(ctx, req.NamespacedName, restoreInstance)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}
	if len(restoreInstance.Status.Conditions) > 0 {
		return ctrl.Result{}, nil
	}
	restoreLogger.Info("Jenkins Restore with name " + restoreInstance.Name + " has been created")

	// Fetch the Backup instance
	backupInstance := &v1alpha2.Backup{}
	backupNamespacedName := types.NamespacedName{
		Name:      restoreInstance.Spec.BackupRef,
		Namespace: req.Namespace,
	}
	err = r.Client.Get(ctx, backupNamespacedName, backupInstance)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	// Fetch the Jenkins instance
	jenkinsInstance := &v1alpha2.Jenkins{}
	backupStrategy := &v1alpha2.BackupStrategy{}
	backupSpec := backupInstance.Spec
	// Use default BackupStrategy if strategyRef not provided
	backupStrategyName := DefaultBackupStrategyName
	if backupSpec.StrategyRef != "" {
		backupStrategyName = backupSpec.StrategyRef
	}
	backupStrategyNamespacedName := types.NamespacedName{
		Namespace: req.Namespace,
		Name:      backupStrategyName,
	}
	err = r.Client.Get(ctx, backupStrategyNamespacedName, backupStrategy)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	jenkinsNamespacedName := types.NamespacedName{
		Name:      backupSpec.JenkinsRef,
		Namespace: req.Namespace,
	}
	err = r.Client.Get(ctx, jenkinsNamespacedName, jenkinsInstance)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}
	restoreLogger.Info(fmt.Sprintf("Restore in progress for Jenkins instance '%s'", jenkinsInstance.Name))
	restoreLogger.Info(fmt.Sprintf("Jenkins '%s' for Restore '%s' found !", jenkinsInstance.Name, req.Name))

	jenkinsPod, err := r.GetPodByDeployment(jenkinsInstance)
	if err != nil {
		return ctrl.Result{}, err
	}

	err = execClient.InitKubeGoClient()
	if err != nil {
		restoreInstance.Status.Conditions.SetCondition(status.Condition{
			Type:   RestoreInitialized,
			Status: corev1.ConditionFalse,
			Reason: (status.ConditionReason)(err.Error()),
		})
		err = r.Client.Status().Update(ctx, restoreInstance)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, err
	}
	restoreInstance.Status.Conditions.SetCondition(status.Condition{
		Type:   RestoreInitialized,
		Status: corev1.ConditionTrue,
	})
	err = r.Client.Status().Update(ctx, restoreInstance)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Restore
	err = r.performJenkinsRestore(ctx, execClient, jenkinsPod, backupInstance, backupStrategy, restoreInstance)
	r.sendNewRestoreInProgressNotification(jenkinsInstance, restoreInstance, "restore", err)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Restart
	if backupStrategy.Spec.RestartAfterRestore.Enabled && backupStrategy.Spec.RestartAfterRestore.Safe {
		err := r.performJenkinsSafeRestart(ctx, jenkinsPod, restoreInstance, execClient)
		r.sendNewRestoreInProgressNotification(jenkinsInstance, restoreInstance, "safeRestartAfterRestore", err)
		if err != nil {
			return ctrl.Result{}, err
		}
	} else if backupStrategy.Spec.RestartAfterRestore.Enabled {
		err := r.performJenkinsRestart(ctx, execClient, jenkinsPod, restoreInstance)
		r.sendNewRestoreInProgressNotification(jenkinsInstance, restoreInstance, "restartAfterRestore", err)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	r.sendNewRestoreCompletedNotification(jenkinsInstance, restoreInstance, err)
	err = r.Client.Status().Update(ctx, restoreInstance)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *RestoreReconciler) performJenkinsRestart(ctx context.Context, execClient exec.KubeExecClient, jenkinsPod *corev1.Pod, restoreInstance *v1alpha2.Restore) error {
	execRestart := strings.Join([]string{"sh", resources.RestartScriptPath}, " ")
	err := execClient.MakeRequest(jenkinsPod, restoreInstance.Name, execRestart)
	if err != nil {
		restoreInstance.Status.Conditions.SetCondition(status.Condition{
			Type:   RestartStarted,
			Status: corev1.ConditionFalse,
			Reason: (status.ConditionReason)(fmt.Sprintf("Failed to restart Jenkins %s", err.Error())),
		})
		err = r.Client.Status().Update(ctx, restoreInstance)
		if err != nil {
			return err
		}
		return nil
	}
	restoreInstance.Status.Conditions.SetCondition(status.Condition{
		Type:   RestartStarted,
		Status: corev1.ConditionTrue,
	})
	return nil
}

func (r *RestoreReconciler) performJenkinsSafeRestart(ctx context.Context, jenkinsPod *corev1.Pod, restoreInstance *v1alpha2.Restore, execClient exec.KubeExecClient) error {
	execSafeRestart := strings.Join([]string{"sh", resources.SafeRestartScriptPath}, " ")
	err := execClient.MakeRequest(jenkinsPod, restoreInstance.Name, execSafeRestart)
	if err != nil {
		restoreInstance.Status.Conditions.SetCondition(status.Condition{
			Type:   SafeRestartStarted,
			Status: corev1.ConditionFalse,
			Reason: (status.ConditionReason)(fmt.Sprintf("Failed to safe restart Jenkins %s", err.Error())),
		})
		err = r.Client.Status().Update(ctx, restoreInstance)
		if err != nil {
			return err
		}
		return nil
	}
	restoreInstance.Status.Conditions.SetCondition(status.Condition{
		Type:   SafeRestartStarted,
		Status: corev1.ConditionTrue,
	})
	return nil
}

func (r *RestoreReconciler) performJenkinsRestore(ctx context.Context, execClient exec.KubeExecClient, jenkinsPod *corev1.Pod, backupInstance *v1alpha2.Backup, backupStrategy *v1alpha2.BackupStrategy, restoreInstance *v1alpha2.Restore) error {
	restoreFromLocation := resources.JenkinsBackupVolumePath + "/" + backupInstance.Spec.BackupVolumeRef + "/" + backupInstance.Name
	restoreToLocation := defaultJenkinsHome
	restoreToSubLocations := []string{}
	if backupStrategy.Spec.Options.Config {
		restoreToSubLocations = append(restoreToSubLocations, "*.xml")
	}
	if backupStrategy.Spec.Options.Jobs {
		restoreToSubLocations = append(restoreToSubLocations, "jobs")
	}
	if backupStrategy.Spec.Options.Plugins {
		restoreToSubLocations = append(restoreToSubLocations, "plugins")
	}
	if len(restoreToSubLocations) > 0 {
		for _, sl := range restoreToSubLocations {
			// Restore each location in a different request
			restoreFromSubLocation := ""
			restoreToSubLocation := ""
			if sl == "*.xml" {
				restoreFromSubLocation = strings.Join([]string{restoreFromLocation, sl}, "/")
				restoreToSubLocation = strings.Join([]string{restoreToLocation, ""}, "/")
			} else {
				restoreFromSubLocation = strings.Join([]string{restoreFromLocation, sl + "/*"}, "/")
				restoreToSubLocation = strings.Join([]string{restoreToLocation, sl}, "/")
			}
			execRestoreSubLocation := strings.Join([]string{"cp", "-r", restoreFromSubLocation, restoreToSubLocation}, " ")
			err := execClient.MakeRequest(jenkinsPod, restoreInstance.Name, execRestoreSubLocation)
			if err != nil {
				restoreInstance.Status.Conditions.SetCondition(status.Condition{
					Type:   RestoreCompleted,
					Status: corev1.ConditionFalse,
					Reason: (status.ConditionReason)(fmt.Sprintf("Failed to restore from %s %s", restoreFromSubLocation, err.Error())),
				})
				err = r.Client.Status().Update(ctx, restoreInstance)
				if err != nil {
					return err
				}
				return nil
			}
		}
		restoreInstance.Status.Conditions.SetCondition(status.Condition{
			Type:   RestoreCompleted,
			Status: corev1.ConditionTrue,
		})
		err := r.Client.Status().Update(ctx, restoreInstance)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *RestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha2.Restore{}).
		Complete(r)
}

func (r *RestoreReconciler) GetPodByDeployment(jenkins *v1alpha2.Jenkins) (*corev1.Pod, error) {
	replicaSet, err := r.GetReplicaSetByDeployment(jenkins)
	if err != nil {
		return nil, err
	}
	selector, err := metav1.LabelSelectorAsSelector(replicaSet.Spec.Selector)
	if err != nil {
		return nil, err
	}
	listOptions := client.ListOptions{LabelSelector: selector}
	pods := corev1.PodList{}
	err = r.Client.List(context.TODO(), &pods, &listOptions)
	if err != nil || len(pods.Items) == 0 {
		return nil, stackerr.Errorf("Deployment has no pod attached yet: Error was: %+v", err)
	}
	pod := pods.Items[0]
	restoreLogger.V(log.VDebug).Info(fmt.Sprintf("Successfully got the Pod: %s", pod.Name))
	return &pods.Items[0], err
}

// GetJenkinsDeployment gets the jenkins master deployment.
func (r *RestoreReconciler) GetJenkinsDeployment(jenkins *v1alpha2.Jenkins) (*appsv1.Deployment, error) {
	deploymentName := resources.GetJenkinsDeploymentName(jenkins)
	restoreLogger.V(log.VDebug).Info(fmt.Sprintf("Getting JenkinsDeploymentName for : %+v, querying deployment named: %s", jenkins.Name, deploymentName))
	jenkinsDeployment := &appsv1.Deployment{}
	namespacedName := types.NamespacedName{Name: deploymentName, Namespace: jenkins.Namespace}
	err := r.Client.Get(context.TODO(), namespacedName, jenkinsDeployment)
	if err != nil {
		restoreLogger.V(log.VDebug).Info(fmt.Sprintf("No deployment named: %s found: %+v", deploymentName, err))
		return nil, err
	}
	return jenkinsDeployment, nil
}

func (r *RestoreReconciler) GetReplicaSetByDeployment(jenkins *v1alpha2.Jenkins) (*appsv1.ReplicaSet, error) {
	deployment, _ := r.GetJenkinsDeployment(jenkins)
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	replicasSetList := appsv1.ReplicaSetList{}
	if err != nil {
		restoreLogger.V(log.VDebug).Info(fmt.Sprintf("Error while getting the replicaset using selector: %s : error: %+v", selector, err))
	}
	listOptions := client.ListOptions{LabelSelector: selector}
	err = r.Client.List(context.TODO(), &replicasSetList, &listOptions)
	if err != nil || len(replicasSetList.Items) == 0 {
		restoreLogger.V(log.VDebug).Info(fmt.Sprintf("Error while getting the replicaset using selector: %s : error: %+v", selector, err))
		return nil, stackerr.Errorf("Deployment has no replicaSet attached yet: Error was: %+v", err)
	}
	replicaSet := replicasSetList.Items[0]
	restoreLogger.V(log.VDebug).Info(fmt.Sprintf("Successfully got the ReplicaSet: %s", replicaSet.Name))
	return &replicaSet, nil
}

func (r *RestoreReconciler) sendNewRestoreCompletedNotification(jenkins *v1alpha2.Jenkins, restore *v1alpha2.Restore, err error) {
	r.NotificationEvents <- event.Event{
		Jenkins:    *jenkins,
		Controller: event.BackupController,
		Level:      v1alpha2.NotificationLevelInfo,
		Reason: reason.NewRestoreCompleted(reason.OperatorSource,
			[]string{fmt.Sprintf("Restore of backup %s completed for Jenkins '%s' in namespace '%s' with err %s", jenkins.Name, restore.Spec.BackupRef, jenkins.Namespace, err)}),
	}
}

func (r *RestoreReconciler) sendNewRestoreInProgressNotification(jenkins *v1alpha2.Jenkins, restore *v1alpha2.Restore, message string, err error) {
	r.NotificationEvents <- event.Event{
		Jenkins:    *jenkins,
		Controller: event.BackupController,
		Level:      v1alpha2.NotificationLevelInfo,
		Reason: reason.NewRestoreInProgress(reason.OperatorSource,
			[]string{fmt.Sprintf("Restore of backup %s in progress for Jenkins '%s' in namespace '%s' with message '%s' err %s", jenkins.Name, restore.Spec.BackupRef, jenkins.Namespace, message, err)}),
	}
}
