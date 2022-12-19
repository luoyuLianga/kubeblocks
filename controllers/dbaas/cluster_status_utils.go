/*
Copyright ApeCloud Inc.

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

package dbaas

import (
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/exp/slices"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbaasv1alpha1 "github.com/apecloud/kubeblocks/apis/dbaas/v1alpha1"
	"github.com/apecloud/kubeblocks/controllers/dbaas/components"
	"github.com/apecloud/kubeblocks/controllers/dbaas/components/util"
	"github.com/apecloud/kubeblocks/controllers/dbaas/operations"
	"github.com/apecloud/kubeblocks/controllers/k8score"
	intctrlutil "github.com/apecloud/kubeblocks/internal/controllerutil"
)

const (
	// EventTimeOut timeout of the event
	EventTimeOut = 30 * time.Second

	// EventOccursTimes occurs times of the event
	EventOccursTimes int32 = 3
)

// isTargetKindForEvent check the event involve object is the target resources
func isTargetKindForEvent(event *corev1.Event) bool {
	return slices.Index([]string{intctrlutil.PodKind, intctrlutil.DeploymentKind, intctrlutil.StatefulSetKind}, event.InvolvedObject.Kind) != -1
}

// isOperationsPhaseForCluster determine whether operations are in progress according to the cluster status except volumeExpanding.
func isOperationsPhaseForCluster(phase dbaasv1alpha1.Phase) bool {
	return slices.Index([]dbaasv1alpha1.Phase{dbaasv1alpha1.CreatingPhase, dbaasv1alpha1.UpdatingPhase}, phase) != -1
}

// getFinalEventMessageForRecorder get final event message by event involved object kind for recorded it
func getFinalEventMessageForRecorder(event *corev1.Event) string {
	if event.InvolvedObject.Kind == intctrlutil.PodKind {
		return fmt.Sprintf("Pod %s: %s", event.InvolvedObject.Name, event.Message)
	}
	return event.Message
}

// isExistsEventMsg check whether the event is exists
func isExistsEventMsg(statusComponentMessage map[string]string, event *corev1.Event) bool {
	if statusComponentMessage == nil {
		return false
	}
	messageKey := util.GetStatusComponentMessageKey(event.InvolvedObject.Kind, event.InvolvedObject.Name)
	if message, ok := statusComponentMessage[messageKey]; !ok {
		return false
	} else {
		return strings.Contains(message, event.Message)
	}

}

// updateStatusComponentMessage update status component message map
func updateStatusComponentMessage(statusComponent *dbaasv1alpha1.ClusterStatusComponent, event *corev1.Event) {
	var (
		kind = event.InvolvedObject.Kind
		name = event.InvolvedObject.Name
	)
	messageMap := statusComponent.GetMessage()
	message := messageMap.GetObjectMessage(kind, name)
	// if the event message is not exists in message map, merge them.
	if message != "" && !strings.Contains(message, event.Message) {
		message += ";" + event.Message
	}
	messageMap.SetObjectMessage(kind, name, message)
	statusComponent.SetMessage(messageMap)
}

// needSyncComponentStatusForEvent check whether the component status needs to be synchronized the cluster status by event
func needSyncComponentStatusForEvent(cluster *dbaasv1alpha1.Cluster, componentName string, phase dbaasv1alpha1.Phase, event *corev1.Event) bool {
	var (
		status          = &cluster.Status
		statusComponent dbaasv1alpha1.ClusterStatusComponent
		ok              bool
	)
	if phase == "" {
		return false
	}
	if cluster.Status.Components == nil {
		status.Components = map[string]dbaasv1alpha1.ClusterStatusComponent{}
	}
	if statusComponent, ok = cluster.Status.Components[componentName]; !ok {
		statusComponent = dbaasv1alpha1.ClusterStatusComponent{Phase: phase}
		updateStatusComponentMessage(&statusComponent, event)
		status.Components[componentName] = statusComponent
		return true
	}
	if statusComponent.Phase != phase {
		statusComponent.Phase = phase
		updateStatusComponentMessage(&statusComponent, event)
		status.Components[componentName] = statusComponent
		return true
	}
	// check whether it is a new warning event and the component phase is running
	if !isExistsEventMsg(statusComponent.Message, event) && phase != dbaasv1alpha1.RunningPhase {
		updateStatusComponentMessage(&statusComponent, event)
		status.Components[componentName] = statusComponent
		return true
	}
	return false
}

// getEventInvolvedObject get event involved object for StatefulSet/Deployment/Pod workload
func getEventInvolvedObject(ctx context.Context, cli client.Client, event *corev1.Event) (client.Object, error) {
	objectKey := client.ObjectKey{
		Name:      event.InvolvedObject.Name,
		Namespace: event.InvolvedObject.Namespace,
	}
	var err error
	// If client.object interface object is used as a parameter, it will not return an error when the object is not found.
	// so we should specify the object type to get the object.
	switch event.InvolvedObject.Kind {
	case intctrlutil.PodKind:
		pod := &corev1.Pod{}
		err = cli.Get(ctx, objectKey, pod)
		return pod, err
	case intctrlutil.StatefulSetKind:
		sts := &appsv1.StatefulSet{}
		err = cli.Get(ctx, objectKey, sts)
		return sts, err
	case intctrlutil.DeploymentKind:
		deployment := &appsv1.Deployment{}
		err = cli.Get(ctx, objectKey, deployment)
		return deployment, err
	}
	return nil, err
}

// handleClusterStatusPhaseByEvent handle the Cluster.status.phase when warning event happened.
func handleClusterStatusPhaseByEvent(cluster *dbaasv1alpha1.Cluster, componentMap map[string]string, clusterAvailabilityMap map[string]bool) {
	var (
		isFailed                       bool
		needSyncClusterPhase           = true
		ReplicasNotReadyComponentNames = make([]string, 0)
		notReadyComponentNames         = make([]string, 0)
	)
	for k, v := range cluster.Status.Components {
		componentType := componentMap[k]
		clusterAvailabilityEffect := clusterAvailabilityMap[componentType]
		// if the component is in Failed phase and the component can affect cluster availability, set Cluster.status.phase to Failed
		if clusterAvailabilityEffect && v.Phase == dbaasv1alpha1.FailedPhase {
			isFailed = true
		}
		// determine whether other components are still doing operation, i.e., create/restart/scaling.
		// if exists operations, it will be handled by cluster controller to sync Cluster.status.phase.
		if isOperationsPhaseForCluster(v.Phase) {
			needSyncClusterPhase = false
		}
		if v.PodsReady == nil || !*v.PodsReady {
			ReplicasNotReadyComponentNames = append(ReplicasNotReadyComponentNames, k)
		}
		if util.IsFailedOrAbnormal(v.Phase) {
			notReadyComponentNames = append(notReadyComponentNames, k)
		}
	}
	// record the not ready conditions in cluster
	if len(ReplicasNotReadyComponentNames) > 0 {
		message := fmt.Sprintf("pods are not ready in Components: %v, refer to related component message in Cluster.status.components", ReplicasNotReadyComponentNames)
		cluster.SetStatusCondition(newReplicasNotReadyCondition(message))
	}
	// record the not ready conditions in cluster
	if len(notReadyComponentNames) > 0 {
		message := fmt.Sprintf("pods are unavailable in Components: %v, refer to related component message in Cluster.status.components", notReadyComponentNames)
		cluster.SetStatusCondition(newComponentsNotReadyCondition(message))
	}
	if !needSyncClusterPhase {
		return
	}
	// if the cluster is not in Failed phase, set Cluster.status.phase to Abnormal
	if isFailed {
		cluster.Status.Phase = dbaasv1alpha1.FailedPhase
	} else if cluster.Status.Phase != dbaasv1alpha1.FailedPhase {
		cluster.Status.Phase = dbaasv1alpha1.AbnormalPhase
	}
}

// getClusterAvailabilityEffect whether the component will affect the cluster availability.
// if the component can affect and be Failed, the cluster will be Failed too.
func getClusterAvailabilityEffect(componentDef *dbaasv1alpha1.ClusterDefinitionComponent) bool {
	switch componentDef.ComponentType {
	case dbaasv1alpha1.Consensus:
		return true
	default:
		// other types of components need to judge whether there has PodDisruptionBudget
		return existsPDBSpec(componentDef.PDBSpec)
	}
}

// getComponentRelatedInfo get componentMap, clusterAvailabilityMap and component definition information
func getComponentRelatedInfo(cluster *dbaasv1alpha1.Cluster, clusterDef *dbaasv1alpha1.ClusterDefinition, componentName string) (map[string]string, map[string]bool, dbaasv1alpha1.ClusterDefinitionComponent) {
	var (
		typeName     string
		componentMap = map[string]string{}
		componentDef dbaasv1alpha1.ClusterDefinitionComponent
	)
	for _, v := range cluster.Spec.Components {
		if v.Name == componentName {
			typeName = v.Type
		}
		componentMap[v.Name] = v.Type
	}
	clusterAvailabilityEffectMap := map[string]bool{}
	for _, v := range clusterDef.Spec.Components {
		clusterAvailabilityEffectMap[v.TypeName] = getClusterAvailabilityEffect(&v)
		if v.TypeName == typeName {
			componentDef = v
		}
	}
	return componentMap, clusterAvailabilityEffectMap, componentDef
}

// handleClusterStatusByEvent handle the cluster status when warning event happened
func handleClusterStatusByEvent(ctx context.Context, cli client.Client, recorder record.EventRecorder, object client.Object, event *corev1.Event) error {
	var (
		cluster    = &dbaasv1alpha1.Cluster{}
		labels     = object.GetLabels()
		clusterDef = &dbaasv1alpha1.ClusterDefinition{}
		phase      dbaasv1alpha1.Phase
		err        error
	)
	if err = cli.Get(ctx, client.ObjectKey{Name: labels[intctrlutil.AppInstanceLabelKey], Namespace: object.GetNamespace()}, cluster); err != nil {
		return err
	}
	if err = cli.Get(ctx, client.ObjectKey{Name: cluster.Spec.ClusterDefRef}, clusterDef); err != nil {
		return err
	}
	componentName := labels[intctrlutil.AppComponentLabelKey]
	// get the component phase by component type and sync to Cluster.status.components
	patch := client.MergeFrom(cluster.DeepCopy())
	componentMap, clusterAvailabilityEffectMap, componentDef := getComponentRelatedInfo(cluster, clusterDef, componentName)
	// get the component status by event and check whether the component status needs to be synchronized to the cluster
	component := components.NewComponentByType(ctx, cli, cluster, &componentDef, componentName)
	if component == nil {
		return nil
	}
	phase, err = component.CalculatePhaseWhenPodsNotReady(componentName)
	if err != nil {
		return err
	}
	if !needSyncComponentStatusForEvent(cluster, componentName, phase, event) {
		return nil
	}
	// handle Cluster.status.phase
	handleClusterStatusPhaseByEvent(cluster, componentMap, clusterAvailabilityEffectMap)
	if err = cli.Status().Patch(ctx, cluster, patch); err != nil {
		return err
	}
	recorder.Eventf(cluster, corev1.EventTypeWarning, event.Reason, getFinalEventMessageForRecorder(event))
	return operations.MarkRunningOpsRequestAnnotation(ctx, cli, cluster)
}

// handleEventForClusterStatus handle event for cluster Warning and Failed phase
func handleEventForClusterStatus(ctx context.Context, cli client.Client, recorder record.EventRecorder, event *corev1.Event) error {
	var (
		err    error
		object client.Object
	)
	if event.Type != corev1.EventTypeWarning || !isTargetKindForEvent(event) {
		return nil
	}
	if !k8score.IsOvertimeAndOccursTimesForEvent(event, EventTimeOut, EventOccursTimes) {
		return nil
	}
	if object, err = getEventInvolvedObject(ctx, cli, event); err != nil {
		return err
	}
	if object == nil || !intctrlutil.WorkloadFilterPredicate(object) {
		return nil
	}
	return handleClusterStatusByEvent(ctx, cli, recorder, object, event)
}
