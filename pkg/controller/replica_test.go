/*
Copyright 2021 The Kubernetes Authors.

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
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/wait"
	fakev1 "k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2/klogr"
	azdiskv1beta2 "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/azuredisk/v1beta2"
	azdiskfakes "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/client/clientset/versioned/fake"
	consts "sigs.k8s.io/azuredisk-csi-driver/pkg/azureconstants"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/azureutils"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/azureutils/mockclient"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	testTimeUntilGarbageCollection = time.Duration(30) * time.Second
)

func NewTestReplicaController(controller *gomock.Controller, namespace string, objects ...runtime.Object) *ReconcileReplica {
	azDiskObjs, kubeObjs := splitObjects(objects...)
	controllerSharedState := initState(mockclient.NewMockClient(controller), azdiskfakes.NewSimpleClientset(azDiskObjs...), fakev1.NewSimpleClientset(kubeObjs...), objects...)

	return &ReconcileReplica{
		SharedState:                controllerSharedState,
		timeUntilGarbageCollection: testTimeUntilGarbageCollection,
		logger:                     klogr.New(),
	}
}

func TestReplicaReconcile(t *testing.T) {
	testTimestamp := time.Now()

	tests := []struct {
		description string
		request     reconcile.Request
		setupFunc   func(*testing.T, *gomock.Controller) *ReconcileReplica
		verifyFunc  func(*testing.T, *ReconcileReplica, reconcile.Result, error)
	}{
		{
			description: "[Success] Should update state if replicas in DetachmentFailed state.",
			request:     testReplicaAzVolumeAttachmentRequest,
			setupFunc: func(t *testing.T, mockCtl *gomock.Controller) *ReconcileReplica {
				replicaAttachment := testReplicaAzVolumeAttachment
				replicaAttachment.Status.Annotations = azureutils.AddToMap(replicaAttachment.Status.Annotations, consts.VolumeDetachRequestAnnotation, string(azdrivernode))
				replicaAttachment.Status.State = azdiskv1beta2.DetachmentFailed

				newVolume := testAzVolume0.DeepCopy()
				newVolume.Status.Detail = &azdiskv1beta2.AzVolumeStatusDetail{
					VolumeID: testManagedDiskURI0,
				}

				controller := NewTestReplicaController(
					mockCtl,
					testNamespace,
					newVolume,
					&testPersistentVolume0,
					&testNode0,
					&testNode1,
					&testPod0,
					&replicaAttachment)

				addTestNodeInAvailableAttachmentsMap(controller.SharedState, testNode0.Name, testNodeAvailableAttachmentCount)
				addTestNodeInAvailableAttachmentsMap(controller.SharedState, testNode1.Name, testNodeAvailableAttachmentCount)

				mockClients(controller.cachedClient.(*mockclient.MockClient), controller.azClient, controller.kubeClient)
				return controller
			},
			verifyFunc: func(t *testing.T, controller *ReconcileReplica, result reconcile.Result, err error) {
				require.NoError(t, err)
				require.False(t, result.Requeue)

				azva, err := controller.azClient.DiskV1beta2().AzVolumeAttachments(testNamespace).Get(context.TODO(), testReplicaAzVolumeAttachmentName, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, azdiskv1beta2.ForceDetachPending, azva.Status.State)
			},
		},
		{
			description: "[Success] Should clean up replica attachments upon primary demotion.",
			request:     testPrimaryAzVolumeAttachment0Request,
			setupFunc: func(t *testing.T, mockCtl *gomock.Controller) *ReconcileReplica {
				primaryAttachment := testPrimaryAzVolumeAttachment0.DeepCopy()
				primaryAttachment.Labels = azureutils.AddToMap(primaryAttachment.Labels, consts.RoleLabel, string(azdiskv1beta2.ReplicaRole), consts.RoleChangeLabel, consts.Demoted)
				primaryAttachment.Status.Detail = &azdiskv1beta2.AzVolumeAttachmentStatusDetail{}
				primaryAttachment.Status.Detail.Role = azdiskv1beta2.PrimaryRole
				primaryAttachment.Spec.RequestedRole = azdiskv1beta2.ReplicaRole

				newVolume := testAzVolume0.DeepCopy()
				newVolume.Status.Detail = &azdiskv1beta2.AzVolumeStatusDetail{
					VolumeID: testManagedDiskURI0,
				}

				controller := NewTestReplicaController(
					mockCtl,
					testNamespace,
					newVolume,
					primaryAttachment,
					&testPersistentVolume0,
					&testNode0,
					&testNode1,
					&testReplicaAzVolumeAttachment)

				addTestNodeInAvailableAttachmentsMap(controller.SharedState, testNode0.Name, testNodeAvailableAttachmentCount)
				addTestNodeInAvailableAttachmentsMap(controller.SharedState, testNode1.Name, testNodeAvailableAttachmentCount)

				mockClients(controller.cachedClient.(*mockclient.MockClient), controller.azClient, controller.kubeClient)
				return controller
			},
			verifyFunc: func(t *testing.T, controller *ReconcileReplica, result reconcile.Result, err error) {
				require.NoError(t, err)
				require.False(t, result.Requeue)

				// wait for the garbage collection to queue
				time.Sleep(controller.timeUntilGarbageCollection + time.Minute)
				roleReq, _ := azureutils.CreateLabelRequirements(consts.RoleLabel, selection.Equals, string(azdiskv1beta2.ReplicaRole))
				labelSelector := labels.NewSelector().Add(*roleReq)
				replicas, localError := controller.azClient.DiskV1beta2().AzVolumeAttachments(testNamespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector.String()})
				require.NoError(t, localError)
				require.NotNil(t, replicas)
				require.Len(t, replicas.Items, 2)

				for _, replica := range replicas.Items {
					require.NotNil(t, replica.Status.Annotations)
					require.Contains(t, replica.Status.Annotations, consts.VolumeDetachRequestAnnotation)
				}
			},
		},
		{
			description: "[Success] Should delete failedAttachment replica with non-retriable error and create a replacement replica.",
			request:     testReplicaAzVolumeAttachmentRequest,
			setupFunc: func(t *testing.T, mockCtl *gomock.Controller) *ReconcileReplica {
				replicaAttachment := testReplicaAzVolumeAttachment.DeepCopy()
				replicaAttachment.CreationTimestamp.Time = testTimestamp
				replicaAttachment.Status.State = azdiskv1beta2.AttachmentFailed

				newVolume := testAzVolume0.DeepCopy()
				newVolume.Status.Detail = &azdiskv1beta2.AzVolumeStatusDetail{
					VolumeID: testManagedDiskURI0,
				}

				controller := NewTestReplicaController(
					mockCtl,
					testNamespace,
					newVolume,
					&testPersistentVolume0,
					&testNode0,
					&testNode1,
					&testPod0,
					replicaAttachment)

				addTestNodeInAvailableAttachmentsMap(controller.SharedState, testNode0.Name, testNodeAvailableAttachmentCount)
				addTestNodeInAvailableAttachmentsMap(controller.SharedState, testNode1.Name, testNodeAvailableAttachmentCount)

				mockClients(controller.cachedClient.(*mockclient.MockClient), controller.azClient, controller.kubeClient)
				return controller
			},
			verifyFunc: func(t *testing.T, controller *ReconcileReplica, result reconcile.Result, err error) {
				require.NoError(t, err)
				require.False(t, result.Requeue)

				conditionFunc := func() (bool, error) {
					roleReq, _ := azureutils.CreateLabelRequirements(consts.RoleLabel, selection.Equals, string(azdiskv1beta2.ReplicaRole))
					labelSelector := labels.NewSelector().Add(*roleReq)
					replicas, localError := controller.azClient.DiskV1beta2().AzVolumeAttachments(testNamespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector.String()})
					if localError != nil {
						return false, localError
					}
					if len(replicas.Items) != 1 {
						return false, nil
					}

					// there should be a new replica attachment
					return !replicas.Items[0].CreationTimestamp.Time.Equal(testTimestamp), nil
				}
				err = wait.PollImmediate(verifyCRIInterval, verifyCRITimeout, conditionFunc)
				require.NoError(t, err)
			},
		},
		{
			description: "[Success] Should create a replacement replica attachment upon replica promotion.",
			request:     testReplicaAzVolumeAttachmentRequest,
			setupFunc: func(t *testing.T, mockCtl *gomock.Controller) *ReconcileReplica {
				replicaAttachment := testReplicaAzVolumeAttachment.DeepCopy()
				replicaAttachment.Status = azdiskv1beta2.AzVolumeAttachmentStatus{
					Detail: &azdiskv1beta2.AzVolumeAttachmentStatusDetail{
						PublishContext: map[string]string{},
						Role:           azdiskv1beta2.ReplicaRole,
					},
					State: azdiskv1beta2.Attached,
				}

				replicaAttachment.Spec.RequestedRole = azdiskv1beta2.PrimaryRole
				replicaAttachment.Labels = map[string]string{consts.RoleLabel: string(azdiskv1beta2.PrimaryRole)}
				replicaAttachment = updateRole(replicaAttachment, azdiskv1beta2.PrimaryRole)

				newVolume := testAzVolume0.DeepCopy()
				newVolume.Status.Detail = &azdiskv1beta2.AzVolumeStatusDetail{
					VolumeID: testManagedDiskURI0,
				}

				controller := NewTestReplicaController(
					mockCtl,
					testNamespace,
					newVolume,
					&testPersistentVolume0,
					&testNode0,
					&testNode1,
					&testPod0,
					replicaAttachment)

				addTestNodeInAvailableAttachmentsMap(controller.SharedState, testNode0.Name, testNodeAvailableAttachmentCount)
				addTestNodeInAvailableAttachmentsMap(controller.SharedState, testNode1.Name, testNodeAvailableAttachmentCount)

				mockClients(controller.cachedClient.(*mockclient.MockClient), controller.azClient, controller.kubeClient)
				return controller
			},
			verifyFunc: func(t *testing.T, controller *ReconcileReplica, result reconcile.Result, err error) {
				require.NoError(t, err)
				require.False(t, result.Requeue)
				conditionFunc := func() (bool, error) {
					roleReq, _ := azureutils.CreateLabelRequirements(consts.RoleLabel, selection.Equals, string(azdiskv1beta2.ReplicaRole))
					labelSelector := labels.NewSelector().Add(*roleReq)
					replicas, localError := controller.azClient.DiskV1beta2().AzVolumeAttachments(testNamespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector.String()})
					require.NoError(t, localError)
					require.NotNil(t, replicas)
					return len(replicas.Items) == 1, nil
				}
				err = wait.PollImmediate(verifyCRIInterval, verifyCRITimeout, conditionFunc)
				require.NoError(t, err)
			},
		},
		{
			description: "[Success] Should clean up replica AzVolumeAttachments upon primary AzVolumeAttachment detach.",
			request:     testPrimaryAzVolumeAttachment0Request,
			setupFunc: func(t *testing.T, mockCtl *gomock.Controller) *ReconcileReplica {
				primaryAttachment := testPrimaryAzVolumeAttachment0.DeepCopy()
				now := metav1.Time{Time: metav1.Now().Add(-1000)}
				primaryAttachment.DeletionTimestamp = &now
				primaryAttachment.Status.Annotations = map[string]string{consts.VolumeDetachRequestAnnotation: "true"}

				newVolume := testAzVolume0.DeepCopy()
				newVolume.Status.Detail = &azdiskv1beta2.AzVolumeStatusDetail{
					VolumeID: testManagedDiskURI0,
				}

				controller := NewTestReplicaController(
					mockCtl,
					testNamespace,
					newVolume,
					primaryAttachment,
					&testPersistentVolume0,
					&testNode0,
					&testNode1,
					&testReplicaAzVolumeAttachment)

				addTestNodeInAvailableAttachmentsMap(controller.SharedState, testNode0.Name, testNodeAvailableAttachmentCount)
				addTestNodeInAvailableAttachmentsMap(controller.SharedState, testNode1.Name, testNodeAvailableAttachmentCount)

				mockClients(controller.cachedClient.(*mockclient.MockClient), controller.azClient, controller.kubeClient)
				return controller
			},
			verifyFunc: func(t *testing.T, controller *ReconcileReplica, result reconcile.Result, err error) {
				require.NoError(t, err)
				require.False(t, result.Requeue)

				// wait for the garbage collection to queue
				time.Sleep(controller.timeUntilGarbageCollection + time.Minute)
				roleReq, _ := azureutils.CreateLabelRequirements(consts.RoleLabel, selection.Equals, string(azdiskv1beta2.ReplicaRole))
				labelSelector := labels.NewSelector().Add(*roleReq)
				replicas, localError := controller.azClient.DiskV1beta2().AzVolumeAttachments(testNamespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector.String()})
				require.NoError(t, localError)
				require.NotNil(t, replicas)
				require.Len(t, replicas.Items, 1)

				for _, replica := range replicas.Items {
					require.NotNil(t, replica.Status.Annotations)
					require.Contains(t, replica.Status.Annotations, consts.VolumeDetachRequestAnnotation)
				}
			},
		},
		{
			description: "[Success] Should not clean up replica AzVolumeAttachments if a new primary is created / promoted.",
			request:     testReplicaAzVolumeAttachmentRequest,
			setupFunc: func(t *testing.T, mockCtl *gomock.Controller) *ReconcileReplica {
				primaryAttachment := testPrimaryAzVolumeAttachment0.DeepCopy()
				now := metav1.Time{Time: metav1.Now().Add(-1000)}
				primaryAttachment.DeletionTimestamp = &now
				primaryAttachment.Status.Annotations = map[string]string{consts.VolumeDetachRequestAnnotation: "true"}

				replicaAttachment := testReplicaAzVolumeAttachment.DeepCopy()
				replicaAttachment.Status = azdiskv1beta2.AzVolumeAttachmentStatus{
					Detail: &azdiskv1beta2.AzVolumeAttachmentStatusDetail{
						PublishContext: map[string]string{},
						Role:           azdiskv1beta2.ReplicaRole,
					},
					State: azdiskv1beta2.Attached,
				}

				newVolume := testAzVolume0.DeepCopy()
				newVolume.Status.Detail = &azdiskv1beta2.AzVolumeStatusDetail{
					VolumeID: testManagedDiskURI0,
				}

				controller := NewTestReplicaController(
					mockCtl,
					testNamespace,
					newVolume,
					primaryAttachment,
					&testPersistentVolume0,
					&testNode0,
					&testNode1,
					&testPod0,
					replicaAttachment)

				addTestNodeInAvailableAttachmentsMap(controller.SharedState, testNode0.Name, testNodeAvailableAttachmentCount)
				addTestNodeInAvailableAttachmentsMap(controller.SharedState, testNode1.Name, testNodeAvailableAttachmentCount)

				mockClients(controller.cachedClient.(*mockclient.MockClient), controller.azClient, controller.kubeClient)

				// start garbage collection
				result, err := controller.Reconcile(context.TODO(), testPrimaryAzVolumeAttachment0Request)
				require.NoError(t, err)
				require.False(t, result.Requeue)

				// fully delete primary
				err = controller.azClient.DiskV1beta2().AzVolumeAttachments(primaryAttachment.Namespace).Delete(context.TODO(), primaryAttachment.Name, metav1.DeleteOptions{})
				require.NoError(t, err)

				// promote replica to primary
				replicaAttachment.Spec.RequestedRole = azdiskv1beta2.PrimaryRole
				replicaAttachment.Labels = map[string]string{consts.RoleLabel: string(azdiskv1beta2.PrimaryRole)}
				replicaAttachment = updateRole(replicaAttachment.DeepCopy(), azdiskv1beta2.PrimaryRole)

				err = controller.cachedClient.Update(context.TODO(), replicaAttachment)
				require.NoError(t, err)

				return controller
			},
			verifyFunc: func(t *testing.T, controller *ReconcileReplica, result reconcile.Result, err error) {
				require.NoError(t, err)
				require.False(t, result.Requeue)

				time.Sleep(controller.timeUntilGarbageCollection + time.Minute)
				roleReq, _ := azureutils.CreateLabelRequirements(consts.RoleLabel, selection.Equals, string(azdiskv1beta2.ReplicaRole))
				labelSelector := labels.NewSelector().Add(*roleReq)
				replicas, localError := controller.azClient.DiskV1beta2().AzVolumeAttachments(testNamespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector.String()})
				require.NoError(t, localError)
				require.NotNil(t, replicas)
				require.Len(t, replicas.Items, 1)

				// clean up should not have happened
				for _, replica := range replicas.Items {
					if replica.Status.Annotations != nil {
						require.NotContains(t, replica.Status.Annotations, consts.VolumeDetachRequestAnnotation)
					}
				}
			},
		},
	}

	for _, test := range tests {
		tt := test
		t.Run(tt.description, func(t *testing.T) {
			mockCtl := gomock.NewController(t)
			defer mockCtl.Finish()
			controller := tt.setupFunc(t, mockCtl)
			result, err := controller.Reconcile(context.TODO(), tt.request)
			tt.verifyFunc(t, controller, result, err)
		})
	}
}
