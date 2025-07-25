/*
Copyright 2019 The Kubernetes Authors.

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

package cnsnodevmattachment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	vmoperatorv1alpha4 "github.com/vmware-tanzu/vm-operator/api/v1alpha4"
	cnstypes "github.com/vmware/govmomi/cns/types"
	"github.com/vmware/govmomi/object"
	vimtypes "github.com/vmware/govmomi/vim25/types"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	csifault "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/fault"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/utils"

	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	cnsoperatorapis "sigs.k8s.io/vsphere-csi-driver/v3/pkg/apis/cnsoperator"
	cnsnodevmattachmentv1alpha1 "sigs.k8s.io/vsphere-csi-driver/v3/pkg/apis/cnsoperator/cnsnodevmattachment/v1alpha1"
	cnsnode "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/node"
	volumes "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/volume"
	cnsvsphere "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/vsphere"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/config"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/prometheus"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/logger"
	k8s "sigs.k8s.io/vsphere-csi-driver/v3/pkg/kubernetes"
	cnsoperatortypes "sigs.k8s.io/vsphere-csi-driver/v3/pkg/syncer/cnsoperator/types"
)

const (
	defaultMaxWorkerThreadsForNodeVMAttach = 10
)

// backOffDuration is a map of cnsnodevmattachment name's to the time after
// which a request for this instance will be requeued.
// Initialized to 1 second for new instances and for instances whose latest
// reconcile operation succeeded.
// If the reconcile fails, backoff is incremented exponentially.
var (
	backOffDuration         map[k8stypes.NamespacedName]time.Duration
	backOffDurationMapMutex = sync.Mutex{}
)

// Add creates a new CnsNodeVmAttachment Controller and adds it to the Manager,
// vSphereSecretConfigInfo and VirtualCenterTypes. The Manager will set fields
// on the Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager, clusterFlavor cnstypes.CnsClusterFlavor,
	configInfo *config.ConfigurationInfo, volumeManager volumes.Manager) error {
	ctx, log := logger.GetNewContextWithLogger()
	if clusterFlavor != cnstypes.CnsClusterFlavorWorkload {
		log.Debug("Not initializing the CnsNodeVmAttachment Controller as its a non-WCP CSI deployment")
		return nil
	}

	// Initializes kubernetes client.
	k8sclient, err := k8s.NewClient(ctx)
	if err != nil {
		log.Errorf("Creating Kubernetes client failed. Err: %v", err)
		return err
	}

	// eventBroadcaster broadcasts events on cnsnodevmattachment instances to
	// the event sink.
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(
		&typedcorev1.EventSinkImpl{
			Interface: k8sclient.CoreV1().Events(""),
		},
	)

	restClientConfig, err := k8s.GetKubeConfig(ctx)
	if err != nil {
		msg := fmt.Sprintf("Failed to initialize rest clientconfig. Error: %+v", err)
		log.Error(msg)
		return err
	}

	vmOperatorClient, err := k8s.NewClientForGroup(ctx, restClientConfig, vmoperatorv1alpha4.GroupName)
	if err != nil {
		msg := fmt.Sprintf("Failed to initialize vmOperatorClient. Error: %+v", err)
		log.Error(msg)
		return err
	}

	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: cnsoperatorapis.GroupName})
	return add(mgr, newReconciler(mgr, configInfo, volumeManager, vmOperatorClient, recorder))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, configInfo *config.ConfigurationInfo,
	volumeManager volumes.Manager, vmOperatorClient client.Client,
	recorder record.EventRecorder) reconcile.Reconciler {
	ctx, _ := logger.GetNewContextWithLogger()
	return &ReconcileCnsNodeVMAttachment{client: mgr.GetClient(), scheme: mgr.GetScheme(),
		configInfo: configInfo, volumeManager: volumeManager,
		vmOperatorClient: vmOperatorClient, nodeManager: cnsnode.GetManager(ctx),
		recorder: recorder}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler.
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	ctx, log := logger.GetNewContextWithLogger()
	maxWorkerThreads := getMaxWorkerThreadsToReconcileCnsNodeVmAttachment(ctx)
	// Create a new controller.
	c, err := controller.New("cnsnodevmattachment-controller", mgr,
		controller.Options{Reconciler: r, MaxConcurrentReconciles: maxWorkerThreads})
	if err != nil {
		log.Errorf("failed to create new CnsNodeVmAttachment controller with error: %+v", err)
		return err
	}

	backOffDuration = make(map[k8stypes.NamespacedName]time.Duration)

	// Watch for changes to primary resource CnsNodeVmAttachment.
	err = c.Watch(source.Kind(
		mgr.GetCache(),
		&cnsnodevmattachmentv1alpha1.CnsNodeVmAttachment{},
		&handler.TypedEnqueueRequestForObject[*cnsnodevmattachmentv1alpha1.CnsNodeVmAttachment]{},
	))
	if err != nil {
		log.Errorf("failed to watch for changes to CnsNodeVmAttachment resource with error: %+v", err)
		return err
	}
	return nil
}

// blank assignment to verify that ReconcileCnsNodeVMAttachment implements
// reconcile.Reconciler.
var _ reconcile.Reconciler = &ReconcileCnsNodeVMAttachment{}

// ReconcileCnsNodeVMAttachment reconciles a CnsNodeVmAttachment object.
type ReconcileCnsNodeVMAttachment struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client           client.Client
	scheme           *runtime.Scheme
	configInfo       *config.ConfigurationInfo
	volumeManager    volumes.Manager
	vmOperatorClient client.Client
	nodeManager      cnsnode.Manager
	recorder         record.EventRecorder
}

// Reconcile reads that state of the cluster for a CnsNodeVMAttachment object
// and makes changes based on the state read and what is in the
// CnsNodeVMAttachment.Spec.
// Note:
// The Controller will requeue the Request to be processed again if the returned
// error is non-nil or Result.Requeue is true. Otherwise, upon completion it
// will remove the work from the queue.
func (r *ReconcileCnsNodeVMAttachment) Reconcile(ctx context.Context,
	request reconcile.Request) (reconcile.Result, error) {
	start := time.Now()
	ctx = logger.NewContextWithLogger(ctx)
	reconcileLog := logger.GetLogger(ctx)
	reconcileLog.Infof("Received Reconcile for request: %q", request.NamespacedName)
	// Start a goroutine to listen for context cancellation
	go func() {
		<-ctx.Done()
		reconcileLog.Infof("context canceled for reconcile for CnsNodeVMAttachment request: %q, error: %v",
			request.NamespacedName, ctx.Err())
	}()
	volumeType := prometheus.PrometheusBlockVolumeType
	var volumeOpType string
	reconcileCnsNodeVMAttachmentInternal := func(internalCtx context.Context) (
		reconcile.Result, string, error) {
		internalCtx = logger.NewContextWithLogger(internalCtx)
		log := logger.GetLogger(internalCtx)
		log.Infof("Started Reconcile for CnsNodeVMAttachment request: %q", request.NamespacedName)
		// Fetch the CnsNodeVmAttachment instance
		instance := &cnsnodevmattachmentv1alpha1.CnsNodeVmAttachment{}
		volumeOpType = prometheus.PrometheusAttachVolumeOpType
		err := r.client.Get(internalCtx, request.NamespacedName, instance)
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Info("CnsNodeVmAttachment resource not found. Ignoring since object must be deleted.")
				return reconcile.Result{}, "", nil
			}
			log.Errorf("Error reading the CnsNodeVmAttachment with name: %q on namespace: %q. Err: %+v",
				request.Name, request.Namespace, err)
			// Error reading the object - return with err.
			return reconcile.Result{}, csifault.CSIInternalFault, err
		}

		// Initialize backOffDuration for the instance, if required.
		backOffDurationMapMutex.Lock()
		var timeout time.Duration
		if _, exists := backOffDuration[request.NamespacedName]; !exists {
			backOffDuration[request.NamespacedName] = time.Second
		}
		timeout = backOffDuration[request.NamespacedName]
		backOffDurationMapMutex.Unlock()
		log.Infof("Reconciling CnsNodeVmAttachment with Request.Name: %q Namespace %q timeout %q seconds",
			request.Name, request.Namespace, timeout)

		// If the CnsNodeVMAttachment instance is already attached and
		// not deleted by the user, remove the instance from the queue.
		if instance.Status.Attached && instance.DeletionTimestamp == nil {
			// This is an upgrade scenarion : In summary, we fetch the SV PVC and check if the
			// CNS PVC protection finalizer exist. If the finalizer does not exist, it incurs that
			// the attachment object was created with an older CSI. Hence we add the
			// CNS PVC protection finalizer on the SV PVC in the current reconciliation loop.
			pvc := &v1.PersistentVolumeClaim{}
			err = r.client.Get(internalCtx, k8stypes.NamespacedName{Name: instance.Spec.VolumeName,
				Namespace: instance.Namespace}, pvc)
			if err != nil {
				msg := fmt.Sprintf("failed to get PVC with volumename: %q on namespace: %q. Err: %+v",
					instance.Spec.VolumeName, instance.Namespace, err)
				recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
				return reconcile.Result{RequeueAfter: timeout}, csifault.CSIApiServerOperationFault, nil
			}
			cnsPvcFinalizerExists := false
			// Check if cnsPvcFinalizerExists already exists.
			for _, finalizer := range pvc.Finalizers {
				if finalizer == cnsoperatortypes.CNSPvcFinalizer {
					cnsPvcFinalizerExists = true
					log.Infof("Finalizer: %q already exists in the PVC with name: %q on namespace: %q.",
						cnsoperatortypes.CNSPvcFinalizer, instance.Spec.VolumeName, instance.Namespace)
					break
				}
			}
			if !cnsPvcFinalizerExists {
				faulttype, err := addFinalizerToPVC(internalCtx, r.client, pvc)
				if err != nil {
					msg := fmt.Sprintf("failed to add %q finalizer on the PVC with volumename: %q on namespace: %q. Err: %+v",
						cnsoperatortypes.CNSPvcFinalizer, instance.Spec.VolumeName, instance.Namespace, err)
					instance.Status.Error = err.Error()
					err = updateCnsNodeVMAttachment(internalCtx, r.client, instance)
					if err != nil {
						log.Errorf("updateCnsNodeVMAttachment failed. err: %v", err)
					}
					recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
					return reconcile.Result{RequeueAfter: timeout}, faulttype, nil
				}
			}
			log.Infof("CnsNodeVmAttachment instance %q status is already attached. Removing from the queue.", instance.Name)
			// Cleanup instance entry from backOffDuration map.
			backOffDurationMapMutex.Lock()
			delete(backOffDuration, request.NamespacedName)
			backOffDurationMapMutex.Unlock()
			return reconcile.Result{}, "", nil
		}

		vcdcMap, err := getVCDatacentersFromConfig(r.configInfo.Cfg)
		if err != nil {
			msg := fmt.Sprintf("failed to find datacenter moref from config for CnsNodeVmAttachment "+
				"request with name: %q on namespace: %q. Err: %+v", request.Name, request.Namespace, err)
			instance.Status.Error = err.Error()
			err = updateCnsNodeVMAttachment(internalCtx, r.client, instance)
			if err != nil {
				log.Errorf("updateCnsNodeVMAttachment failed. err: %v", err)
			}
			recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
			return reconcile.Result{RequeueAfter: timeout}, csifault.CSIDatacenterNotFoundFault, nil
		}
		var host, dcMoref string
		for key, value := range vcdcMap {
			host = key
			dcMoref = value[0]
		}
		// Get node VM by nodeUUID.
		var dc *cnsvsphere.Datacenter
		vcenter, err := cnsvsphere.GetVirtualCenterInstance(internalCtx, r.configInfo, false)
		if err != nil {
			msg := fmt.Sprintf("failed to get virtual center instance with error: %v", err)
			instance.Status.Error = err.Error()
			err = updateCnsNodeVMAttachment(internalCtx, r.client, instance)
			if err != nil {
				log.Errorf("updateCnsNodeVMAttachment failed. err: %v", err)
			}
			recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
			return reconcile.Result{RequeueAfter: timeout}, csifault.CSIVCenterNotFoundFault, nil
		}
		err = vcenter.Connect(internalCtx)
		if err != nil {
			msg := fmt.Sprintf("failed to connect to VC with error: %v", err)
			instance.Status.Error = err.Error()
			err = updateCnsNodeVMAttachment(internalCtx, r.client, instance)
			if err != nil {
				log.Errorf("updateCnsNodeVMAttachment failed. err: %v", err)
			}
			recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
			return reconcile.Result{RequeueAfter: timeout}, csifault.CSIInternalFault, nil
		}
		dc = &cnsvsphere.Datacenter{
			Datacenter: object.NewDatacenter(vcenter.Client.Client,
				vimtypes.ManagedObjectReference{
					Type:  "Datacenter",
					Value: dcMoref,
				}),
			VirtualCenterHost: host,
		}
		nodeUUID := instance.Spec.NodeUUID
		if !instance.Status.Attached && instance.DeletionTimestamp == nil {
			nodeVM, err := dc.GetVirtualMachineByUUID(internalCtx, nodeUUID, false)
			if err != nil {
				msg := fmt.Sprintf("failed to find the VM with UUID: %q for CnsNodeVmAttachment "+
					"request with name: %q on namespace: %q. Err: %+v",
					nodeUUID, request.Name, request.Namespace, err)
				instance.Status.Error = fmt.Sprintf("Failed to find the VM with UUID: %q", nodeUUID)
				err = updateCnsNodeVMAttachment(internalCtx, r.client, instance)
				if err != nil {
					log.Errorf("updateCnsNodeVMAttachment failed. err: %v", err)
				}
				recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
				return reconcile.Result{RequeueAfter: timeout}, csifault.CSIVmNotFoundFault, nil
			}
			volumeID, faulttype, err := getVolumeID(internalCtx, r.client, instance.Spec.VolumeName, instance.Namespace)
			if err != nil {
				msg := fmt.Sprintf("Failed to get volumeID. Error: %s", err)
				instance.Status.Error = err.Error()
				err = updateCnsNodeVMAttachment(internalCtx, r.client, instance)
				if err != nil {
					log.Errorf("updateCnsNodeVMAttachment failed. err: %v", err)
				}
				recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
				return reconcile.Result{RequeueAfter: timeout}, faulttype, nil
			}

			err = addCNSFinalizer(internalCtx, r.client, instance)
			if err != nil {
				msg := fmt.Sprintf("failed to patch finalizers on CnsNodeVmAttachment instance: %q on namespace: %q. Error: %s",
					request.Name, request.Namespace, err)
				log.Errorf(msg)
				recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
				return reconcile.Result{RequeueAfter: timeout}, csifault.CSIInternalFault, nil
			}

			log.Debugf("instance after the patch: %s", instance)
			log.Infof("vSphere CSI driver is attaching volume: %q to nodevm: %+v for "+
				"CnsNodeVmAttachment request with name: %q on namespace: %q",
				volumeID, nodeVM, request.Name, request.Namespace)
			diskUUID, faulttype, attachErr := r.volumeManager.AttachVolume(internalCtx, nodeVM, volumeID, false)

			if attachErr != nil {
				log.Errorf("failed to attach disk: %q to nodevm: %+v for CnsNodeVmAttachment "+
					"request with name: %q on namespace: %q. Err: %+v",
					volumeID, nodeVM, request.Name, request.Namespace, attachErr)
			}

			pvc := &v1.PersistentVolumeClaim{}
			err = r.client.Get(internalCtx, k8stypes.NamespacedName{Name: instance.Spec.VolumeName,
				Namespace: instance.Namespace}, pvc)
			if err != nil {
				msg := fmt.Sprintf("failed to get PVC with volumename: %q on namespace: %q. Err: %+v",
					instance.Spec.VolumeName, instance.Namespace, err)
				recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
				return reconcile.Result{RequeueAfter: timeout}, csifault.CSIApiServerOperationFault, nil
			}
			cnsPvcFinalizerExists := false
			// Check if cnsPvcFinalizerExists already exists.
			for _, finalizer := range pvc.Finalizers {
				if finalizer == cnsoperatortypes.CNSPvcFinalizer {
					cnsPvcFinalizerExists = true
					break
				}
			}
			if !cnsPvcFinalizerExists {
				faulttype, err = addFinalizerToPVC(internalCtx, r.client, pvc)
				if err != nil {
					msg := fmt.Sprintf("failed to add %q finalizer on the PVC with volumename: %q on namespace: %q. Err: %+v",
						cnsoperatortypes.CNSPvcFinalizer, instance.Spec.VolumeName, instance.Namespace, err)
					instance.Status.Error = err.Error()
					err = updateCnsNodeVMAttachment(internalCtx, r.client, instance)
					if err != nil {
						log.Errorf("updateCnsNodeVMAttachment failed. err: %v", err)
					}
					recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
					return reconcile.Result{RequeueAfter: timeout}, csifault.CSIInternalFault, nil
				}
			}
			if attachErr != nil {
				// Update CnsNodeVMAttachment instance with attach error message.
				instance.Status.Error = attachErr.Error()
			} else {
				// Add the CNS volume ID in the attachment metadata. This is used later
				// to detach the CNS volume on deletion of CnsNodeVMAttachment instance.
				// Note that the supervisor PVC can be deleted due to following:
				// 1. Bug in external provisioner(https://github.com/kubernetes/kubernetes/issues/84226)
				//    where DeleteVolume could be invoked in pvcsi before ControllerUnpublishVolume.
				//    This causes supervisor PVC to be deleted.
				// 2. Supervisor namespace user deletes PVC used by a guest cluster.
				// 3. Supervisor namespace is deleted
				// Basically, we cannot rely on the existence of PVC in supervisor
				// cluster for detaching the volume from guest cluster VM. So, the
				// logic stores the CNS volume ID in attachmentMetadata itself which
				// is used during detach.
				// Update CnsNodeVMAttachment instance with attached status set to true
				// and attachment metadata.
				instance.Status.AttachmentMetadata = make(map[string]string)
				instance.Status.AttachmentMetadata[cnsnodevmattachmentv1alpha1.AttributeCnsVolumeID] = volumeID
				instance.Status.AttachmentMetadata[cnsnodevmattachmentv1alpha1.AttributeFirstClassDiskUUID] = diskUUID
				instance.Status.Attached = true
				// Clear the error message.
				instance.Status.Error = ""
			}

			err = updateCnsNodeVMAttachment(internalCtx, r.client, instance)
			if err != nil {
				msg := fmt.Sprintf("failed to update attach status on CnsNodeVmAttachment "+
					"instance: %q on namespace: %q. Error: %+v",
					request.Name, request.Namespace, err)
				recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
				return reconcile.Result{RequeueAfter: timeout}, csifault.CSIApiServerOperationFault, nil
			}

			if attachErr != nil {
				recordEvent(internalCtx, r, instance, v1.EventTypeWarning, "")
				return reconcile.Result{RequeueAfter: timeout}, faulttype, nil
			}

			msg := fmt.Sprintf("ReconcileCnsNodeVMAttachment: Successfully updated entry in CNS for instance "+
				"with name %q and namespace %q.", request.Name, request.Namespace)
			recordEvent(internalCtx, r, instance, v1.EventTypeNormal, msg)
			// Cleanup instance entry from backOffDuration map.
			backOffDurationMapMutex.Lock()
			delete(backOffDuration, request.NamespacedName)
			backOffDurationMapMutex.Unlock()
			return reconcile.Result{}, "", nil
		}

		if instance.DeletionTimestamp != nil {
			pvc := &v1.PersistentVolumeClaim{}
			var pvcDeleted bool
			err = r.client.Get(internalCtx, k8stypes.NamespacedName{Name: instance.Spec.VolumeName,
				Namespace: instance.Namespace}, pvc)
			if err != nil {
				if apierrors.IsNotFound(err) {
					pvcDeleted = true
				} else {
					msg := fmt.Sprintf("failed to get PVC with volumename: %q on namespace: %q. Err: %+v",
						instance.Spec.VolumeName, instance.Namespace, err)
					recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
					return reconcile.Result{RequeueAfter: timeout}, csifault.CSIApiServerOperationFault, nil
				}
			}
			volumeOpType = prometheus.PrometheusDetachVolumeOpType
			nodeVM, err := dc.GetVirtualMachineByUUID(internalCtx, nodeUUID, false)
			if err != nil {
				msg := fmt.Sprintf("failed to find the VM on VC with UUID: %s for "+
					"CnsNodeVmAttachment request with name: %q on namespace: %s. Err: %+v",
					nodeUUID, request.Name, request.Namespace, err)
				log.Infof(msg)
				if err != cnsvsphere.ErrVMNotFound {
					msg := fmt.Sprintf("VM on VC with UUID: %s not found when processing "+
						"CnsNodeVmAttachment request with name: %q on namespace: %q",
						nodeUUID, request.Name, request.Namespace)
					recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
					return reconcile.Result{RequeueAfter: timeout}, csifault.CSIFindVmByUUIDFault, nil
				}
				// Now that VM on VC is not found, check VirtualMachine CRD instance exists.
				// This check is needed in scenarios where VC inventory is stale due
				// to upgrade or back-up and restore.
				vmInstance, err := isVmCrPresent(internalCtx, r.vmOperatorClient, nodeUUID,
					request.Namespace)
				if err != nil {
					msg = fmt.Sprintf("failed to verify is VM CR is present with UUID: %s "+
						"in namespace: %s", nodeUUID, request.Namespace)
					recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
					return reconcile.Result{RequeueAfter: timeout}, csifault.CSIApiServerOperationFault, nil
				}
				if vmInstance == nil {
					// This is a case where VirtualMachine is not present on the VC and VM CR
					// is also not found in the API server. The detach will be marked as
					// successful in CnsNodeVmAttachment
					msg = fmt.Sprintf("VM CR is not present with UUID: %s in namespace: %s. "+
						"Removing finalizer on CnsNodeVMAttachment: %s instance.",
						nodeUUID, request.Namespace, request.Name)
				} else {
					// This is a case where VirtualMachine is not present on the VC and VM CR
					// has the deletionTimestamp set. The CnsNodeVmAttachment
					// can be marked as a success since the VM CR has deletionTimestamp set
					if vmInstance.DeletionTimestamp != nil {
						msg = fmt.Sprintf("VM on VC not found but VM CR with UUID: %s "+
							"is still present in namespace: %s and is being deleted. "+
							"Hence returning success.", nodeUUID, request.Namespace)
					} else {
						// This is a case where VirtualMachine is not present on the VC and VM CR
						// does not have the deletionTimestamp set. We will record this as a
						// non-storage problem and detach operation will be retried.
						msg = fmt.Sprintf("VM on VC not found but VM CR with UUID: %s "+
							"is still present in namespace: %s. Retrying the operation since "+
							"VM CR is not being deleted.", nodeUUID, request.Namespace)
						recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
						return reconcile.Result{RequeueAfter: timeout}, csifault.CSIVmNotFoundFault, nil
					}
				}
				var faulttype string
				if !pvcDeleted {
					faulttype, err = removeFinalizerFromPVC(internalCtx, r.client, pvc)
					if err != nil {
						msg := fmt.Sprintf("failed to remove %q finalizer on the PVC with volumename: %q on namespace: %q. Err: %+v",
							cnsoperatortypes.CNSPvcFinalizer, instance.Spec.VolumeName, instance.Namespace, err)
						recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
						return reconcile.Result{RequeueAfter: timeout}, faulttype, nil
					}
				}
				removeFinalizerFromCRDInstance(internalCtx, instance, request)
				err = updateCnsNodeVMAttachment(internalCtx, r.client, instance)
				if err != nil {
					log.Errorf("updateCnsNodeVMAttachment failed. err: %v", err)
				}
				recordEvent(internalCtx, r, instance, v1.EventTypeNormal, msg)
				return reconcile.Result{}, faulttype, nil
			}
			var cnsVolumeID string
			var ok bool
			if cnsVolumeID, ok = instance.Status.AttachmentMetadata[cnsnodevmattachmentv1alpha1.AttributeCnsVolumeID]; !ok {
				log.Debugf("CnsNodeVmAttachment does not have CNS volume ID. AttachmentMetadata: %+v",
					instance.Status.AttachmentMetadata)
				msg := "CnsNodeVmAttachment does not have CNS volume ID."
				recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
				return reconcile.Result{RequeueAfter: timeout}, csifault.CSIInternalFault, nil
			}
			log.Infof("vSphere CSI driver is detaching volume: %q to nodevm: %+v for "+
				"CnsNodeVmAttachment request with name: %q on namespace: %q",
				cnsVolumeID, nodeVM, request.Name, request.Namespace)
			faulttype, detachErr := r.volumeManager.DetachVolume(internalCtx, nodeVM, cnsVolumeID)
			if detachErr != nil {
				if cnsvsphere.IsManagedObjectNotFound(detachErr, nodeVM.VirtualMachine.Reference()) {
					msg := fmt.Sprintf("Found a managed object not found fault for vm: %+v", nodeVM)
					if !pvcDeleted {
						faulttype, err = removeFinalizerFromPVC(internalCtx, r.client, pvc)
						if err != nil {
							msg := fmt.Sprintf("failed to remove %q finalizer on the PVC with volumename: %q on namespace: %q. Err: %+v",
								cnsoperatortypes.CNSPvcFinalizer, instance.Spec.VolumeName, instance.Namespace, err)
							recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
							return reconcile.Result{RequeueAfter: timeout}, faulttype, nil
						}
					}
					removeFinalizerFromCRDInstance(internalCtx, instance, request)
					err = updateCnsNodeVMAttachment(internalCtx, r.client, instance)
					if err != nil {
						log.Errorf("updateCnsNodeVMAttachment failed. err: %v", err)
					}
					recordEvent(internalCtx, r, instance, v1.EventTypeNormal, msg)
					// Cleanup instance entry from backOffDuration map.
					backOffDurationMapMutex.Lock()
					delete(backOffDuration, request.NamespacedName)
					backOffDurationMapMutex.Unlock()
					return reconcile.Result{}, "", nil
				}
				// Update CnsNodeVMAttachment instance with detach error message.
				log.Errorf("failed to detach disk: %q to nodevm: %+v for CnsNodeVmAttachment "+
					"request with name: %q on namespace: %q. Err: %+v",
					cnsVolumeID, nodeVM, request.Name, request.Namespace, detachErr)
				instance.Status.Error = detachErr.Error()
			} else {
				if !pvcDeleted {
					faulttype, err = removeFinalizerFromPVC(internalCtx, r.client, pvc)
					if err != nil {
						msg := fmt.Sprintf("failed to remove %q finalizer on the PVC with volumename: %q on namespace: %q. Err: %+v",
							cnsoperatortypes.CNSPvcFinalizer, instance.Spec.VolumeName, instance.Namespace, err)
						recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
						return reconcile.Result{RequeueAfter: timeout}, faulttype, nil
					}
				}
				removeFinalizerFromCRDInstance(internalCtx, instance, request)
			}
			err = updateCnsNodeVMAttachment(internalCtx, r.client, instance)
			if err != nil {
				msg := fmt.Sprintf("failed to update detach status on CnsNodeVmAttachment "+
					"instance: %q on namespace: %q. Error: %+v",
					request.Name, request.Namespace, err)
				recordEvent(internalCtx, r, instance, v1.EventTypeWarning, msg)
				return reconcile.Result{RequeueAfter: timeout}, csifault.CSIApiServerOperationFault, nil
			}
			if detachErr != nil {
				recordEvent(internalCtx, r, instance, v1.EventTypeWarning, "")
				return reconcile.Result{RequeueAfter: timeout}, faulttype, nil
			}
			msg := fmt.Sprintf("ReconcileCnsNodeVMAttachment: Successfully updated entry in CNS for instance "+
				"with name %q and namespace %q.", request.Name, request.Namespace)
			recordEvent(internalCtx, r, instance, v1.EventTypeNormal, msg)
		}
		// Cleanup instance entry from backOffDuration map.
		backOffDurationMapMutex.Lock()
		delete(backOffDuration, request.NamespacedName)
		backOffDurationMapMutex.Unlock()
		log.Infof("Finished Reconcile for CnsNodeVMAttachment request: %q", request.NamespacedName)
		return reconcile.Result{}, "", nil
	}
	// creating new context for reconcileCnsNodeVMAttachmentInternal, as kubernetes supplied context can get canceled
	// This is required to ensure CNS operations won't get prematurely canceled by the controller runtime’s
	// internal reconcile logic.
	newctx, cancel := context.WithTimeout(context.Background(), volumes.VolumeOperationTimeoutInSeconds*time.Second)
	defer cancel()
	resp, faulttype, err := reconcileCnsNodeVMAttachmentInternal(newctx)

	if err != nil || faulttype != "" {
		// When faultype is set, it indicates the attach/detach failure
		// Case 1:
		// both err and faultype are set
		// Case 2:
		// err is nil but faultype type is set
		// This can happen when reconciler returns reconcile.Result{RequeueAfter: timeout}, the err will be set to nil,
		// and corresponding faulttype will be set
		// for this case, we need count it as an attach/detach failure
		if csifault.IsNonStorageFault(faulttype) {
			faulttype = csifault.AddCsiNonStoragePrefix(ctx, faulttype)
		}
		reconcileLog.Errorf("Operation failed, reporting failure status to Prometheus."+
			" Operation Type: %q, Volume Type: %q, Fault Type: %q",
			volumeOpType, volumeType, faulttype)
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, volumeOpType,
			prometheus.PrometheusFailStatus, faulttype).Observe(time.Since(start).Seconds())
	} else {
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, volumeOpType,
			prometheus.PrometheusPassStatus, "").Observe(time.Since(start).Seconds())
	}
	reconcileLog.Infof("Reconcile for request: %q End.", request.NamespacedName)
	return resp, err
}

// removeFinalizerFromCRDInstance will remove the CNS Finalizer, cns.vmware.com,
// from a given nodevmattachment instance.
func removeFinalizerFromCRDInstance(ctx context.Context,
	instance *cnsnodevmattachmentv1alpha1.CnsNodeVmAttachment, request reconcile.Request) {
	log := logger.GetLogger(ctx)
	for i, finalizer := range instance.Finalizers {
		if finalizer == cnsoperatortypes.CNSFinalizer {
			log.Debugf("Removing %q finalizer from CnsNodeVmAttachment instance with name: %q on namespace: %q",
				cnsoperatortypes.CNSFinalizer, request.Name, request.Namespace)
			instance.Finalizers = append(instance.Finalizers[:i], instance.Finalizers[i+1:]...)
		}
	}
}

// addFinalizerToPVC will add the CNS Finalizer, cns.vmware.com/pvc-protection,
// from a given PersistentVolumeClaim.
func addFinalizerToPVC(ctx context.Context, client client.Client,
	pvc *v1.PersistentVolumeClaim) (string, error) {
	log := logger.GetLogger(ctx)
	pvc.Finalizers = append(pvc.Finalizers, cnsoperatortypes.CNSPvcFinalizer)
	log.Infof("Adding %q finalizer on PersistentVolumeClaim: %q on namespace: %q",
		cnsoperatortypes.CNSPvcFinalizer, pvc.Name, pvc.Namespace)
	faulttype, err := updateSVPVC(ctx, client, pvc, false)
	if err != nil {
		log.Errorf("failed to update PersistentVolumeClaim: %q on namespace: %q. Error: %+v",
			pvc.Name, pvc.Namespace, err)
	}
	return faulttype, err
}

// removeFinalizerFromPVC will remove the CNS Finalizer, cns.vmware.com/pvc-protection,
// from a given PersistentVolumeClaim.
func removeFinalizerFromPVC(ctx context.Context, client client.Client,
	pvc *v1.PersistentVolumeClaim) (string, error) {
	log := logger.GetLogger(ctx)
	finalizerFound := false
	for i, finalizer := range pvc.Finalizers {
		if finalizer == cnsoperatortypes.CNSPvcFinalizer {
			log.Debugf("Removing %q finalizer from PersistentVolumeClaim: %q on namespace: %q",
				cnsoperatortypes.CNSPvcFinalizer, pvc.Name, pvc.Namespace)
			pvc.Finalizers = append(pvc.Finalizers[:i], pvc.Finalizers[i+1:]...)
			finalizerFound = true
			break
		}
	}
	if !finalizerFound {
		log.Debugf("Finalizer: %q not found on PersistentVolumeClaim: %q on namespace: %q not found. Returning nil",
			cnsoperatortypes.CNSPvcFinalizer, pvc.Name, pvc.Namespace)
		return "", nil
	}
	faulttype, err := updateSVPVC(ctx, client, pvc, true)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Infof("PersistentVolumeClaim: %q on namespace: %q not found. Returning nil", pvc.Name, pvc.Namespace)
			return "", nil
		}
		log.Errorf("failed to update PersistentVolumeClaim: %q on namespace: %q. Error: %+v",
			pvc.Name, pvc.Namespace, err)
	}
	return faulttype, err

}

func updateSVPVC(ctx context.Context, client client.Client,
	pvc *v1.PersistentVolumeClaim, removeCnsPvcFinalizer bool) (string, error) {
	log := logger.GetLogger(ctx)
	err := client.Update(ctx, pvc)
	if err != nil {
		if apierrors.IsConflict(err) {
			log.Infof("Observed conflict while updating the SV PVC %q in namespace %q."+
				"Reapplying changes to the latest SV PVC object.", pvc.Name, pvc.Namespace)

			// Fetch the latest pvc object from the API server and apply changes on top of it.
			latestPVCObject := &v1.PersistentVolumeClaim{}
			err = client.Get(ctx, k8stypes.NamespacedName{Name: pvc.Name, Namespace: pvc.Namespace}, latestPVCObject)
			if err != nil {
				log.Errorf("Error fetching the SV PVC with name: %q on namespace: %q. Err: %+v",
					pvc.Name, pvc.Namespace, err)
				return csifault.CSIApiServerOperationFault, err
			}

			// The callers of updateSVPVC are only updating the instance finalizers
			// Hence we add/remove the finalizers on the latest PVC object from API server.
			if removeCnsPvcFinalizer {
				for i, finalizer := range latestPVCObject.Finalizers {
					if finalizer == cnsoperatortypes.CNSPvcFinalizer {
						log.Debugf("Removing %q finalizer from PersistentVolumeClaim: %q on namespace: %q",
							cnsoperatortypes.CNSPvcFinalizer, pvc.Name, pvc.Namespace)
						latestPVCObject.Finalizers = append(latestPVCObject.Finalizers[:i], latestPVCObject.Finalizers[i+1:]...)
						break
					}
				}
			} else {
				latestPVCObject.Finalizers = append(latestPVCObject.Finalizers, cnsoperatortypes.CNSPvcFinalizer)
			}
			err := client.Update(ctx, latestPVCObject)
			if err != nil {
				if apierrors.IsConflict(err) {
					log.Infof("Observed conflict again, while updating the SV PVC %q in namespace %q."+
						"Returning error and setting the faulttype as nonStorage fault as the next reconciliation "+
						"will be invoked.", pvc.Name, pvc.Namespace)
					return csifault.CSIResourceUpdateConflictFault, err
				} else {
					log.Errorf("failed to update SV PVC : %q on namespace: %q. Error: %+v",
						pvc.Name, pvc.Namespace, err)
					return csifault.CSIApiServerOperationFault, err
				}
			}
		} else {
			log.Errorf("failed to update SV PVC : %q on namespace: %q. Error: %+v",
				pvc.Name, pvc.Namespace, err)
			return csifault.CSIApiServerOperationFault, err
		}
	}
	return "", nil
}

// isVmCrPresent checks whether VM CR is present in SV namespace
// with given vmuuid and returns the VirtualMachine CR object if it is found
func isVmCrPresent(ctx context.Context, vmOperatorClient client.Client,
	vmuuid string, namespace string) (*vmoperatorv1alpha4.VirtualMachine, error) {
	log := logger.GetLogger(ctx)
	vmList, err := utils.ListVirtualMachines(ctx, vmOperatorClient, namespace)
	if err != nil {
		msg := fmt.Sprintf("failed to list virtualmachines with error: %+v", err)
		log.Error(msg)
		return nil, err
	}
	for _, vmInstance := range vmList.Items {
		if vmInstance.Status.BiosUUID == vmuuid {
			msg := fmt.Sprintf("VM CR with BiosUUID: %s found in namespace: %s",
				vmuuid, namespace)
			log.Infof(msg)
			return &vmInstance, nil
		}
	}
	msg := fmt.Sprintf("VM CR with BiosUUID: %s not found in namespace: %s",
		vmuuid, namespace)
	log.Info(msg)
	return nil, nil
}

// getVCDatacenterFromConfig returns datacenter registered for each vCenter
func getVCDatacentersFromConfig(cfg *config.Config) (map[string][]string, error) {
	var err error
	vcdcMap := make(map[string][]string)
	for key, value := range cfg.VirtualCenter {
		dcList := strings.Split(value.Datacenters, ",")
		for _, dc := range dcList {
			dcMoID := strings.TrimSpace(dc)
			if dcMoID != "" {
				vcdcMap[key] = append(vcdcMap[key], dcMoID)
			}
		}
	}
	if len(vcdcMap) == 0 {
		err = errors.New("unable get vCenter datacenters from vsphere config")
	}
	return vcdcMap, err
}

// getVolumeID gets the volume ID from the PV that is bound to PVC by pvcName.
func getVolumeID(ctx context.Context, client client.Client, pvcName string,
	namespace string) (string, string, error) {
	log := logger.GetLogger(ctx)
	// Get PVC by pvcName from namespace.
	pvc := &v1.PersistentVolumeClaim{}
	err := client.Get(ctx, k8stypes.NamespacedName{Name: pvcName, Namespace: namespace}, pvc)
	if err != nil {
		log.Errorf("failed to get PVC with volumename: %q on namespace: %q. Err: %s",
			pvcName, namespace, err)
		return "", csifault.CSIApiServerOperationFault, err
	}

	// Check if PVC is bound to a PV.
	if pvc.Spec.VolumeName == "" || pvc.Status.Phase != v1.ClaimBound {
		err := fmt.Errorf("PVC with name: %q in namespace: %q is not bound to a PV. "+
			"PV: %q, Status: %q", pvcName, namespace, pvc.Spec.VolumeName, pvc.Status.Phase)
		log.Warn(err.Error())
		return "", csifault.CSIPvNotFoundInPvcSpecFault, err
	}

	// Get PV by name.
	pv := &v1.PersistentVolume{}
	err = client.Get(ctx, k8stypes.NamespacedName{Name: pvc.Spec.VolumeName, Namespace: ""}, pv)
	if err != nil {
		log.Errorf("failed to get PV with name: %q for PVC: %q. Err: %s",
			pvc.Spec.VolumeName, pvcName, err)
		return "", csifault.CSIPvNotFoundInPvcSpecFault, err
	}

	return pv.Spec.CSI.VolumeHandle, "", nil
}

func updateCnsNodeVMAttachment(ctx context.Context, client client.Client,
	instance *cnsnodevmattachmentv1alpha1.CnsNodeVmAttachment) error {
	log := logger.GetLogger(ctx)
	err := client.Update(ctx, instance)
	if err != nil {
		if apierrors.IsConflict(err) {
			log.Infof("Observed conflict while updating CnsNodeVmAttachment instance %q in namespace %q."+
				"Reapplying changes to the latest instance.", instance.Name, instance.Namespace)

			// Fetch the latest instance version from the API server and apply changes on top of it.
			latestInstance := &cnsnodevmattachmentv1alpha1.CnsNodeVmAttachment{}
			err = client.Get(ctx, k8stypes.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, latestInstance)
			if err != nil {
				log.Errorf("Error reading the CnsNodeVmAttachment with name: %q on namespace: %q. Err: %+v",
					instance.Name, instance.Namespace, err)
				// Error reading the object - return error
				return err
			}

			// The callers of updateCnsNodeVMAttachment are either updating the instance finalizers or
			// one of the fields in instance status.
			// Hence we copy only finalizers and Status from the instance passed for update
			// on the latest instance from API server.
			latestInstance.Finalizers = instance.Finalizers
			latestInstance.Status = *instance.Status.DeepCopy()

			err := client.Update(ctx, latestInstance)
			if err != nil {
				log.Errorf("failed to update CnsNodeVmAttachment instance: %q on namespace: %q. Error: %+v",
					instance.Name, instance.Namespace, err)
				return err
			}
			return nil
		} else {
			log.Errorf("failed to update CnsNodeVmAttachment instance: %q on namespace: %q. Error: %+v",
				instance.Name, instance.Namespace, err)
		}
	}
	return err
}

// getMaxWorkerThreadsToReconcileCnsNodeVmAttachment returns the maximum
// number of worker threads which can be run to reconcile CnsNodeVmAttachment
// instances. If environment variable WORKER_THREADS_NODEVM_ATTACH is set and
// valid, return the value read from environment variable otherwise, use the
// default value.
func getMaxWorkerThreadsToReconcileCnsNodeVmAttachment(ctx context.Context) int {
	log := logger.GetLogger(ctx)
	workerThreads := defaultMaxWorkerThreadsForNodeVMAttach
	if v := os.Getenv("WORKER_THREADS_NODEVM_ATTACH"); v != "" {
		if value, err := strconv.Atoi(v); err == nil {
			if value <= 0 {
				log.Warnf("Maximum number of worker threads to run set in env variable "+
					"WORKER_THREADS_NODEVM_ATTACH %s is less than 1, will use the default value %d",
					v, defaultMaxWorkerThreadsForNodeVMAttach)
			} else if value > defaultMaxWorkerThreadsForNodeVMAttach {
				log.Warnf("Maximum number of worker threads to run set in env variable "+
					"WORKER_THREADS_NODEVM_ATTACH %s is greater than %d, will use the default value %d",
					v, defaultMaxWorkerThreadsForNodeVMAttach, defaultMaxWorkerThreadsForNodeVMAttach)
			} else {
				workerThreads = value
				log.Debugf("Maximum number of worker threads to run to reconcile CnsNodeVmAttachment "+
					"instances is set to %d", workerThreads)
			}
		} else {
			log.Warnf("Maximum number of worker threads to run set in env variable "+
				"WORKER_THREADS_NODEVM_ATTACH %s is invalid, will use the default value %d",
				v, defaultMaxWorkerThreadsForNodeVMAttach)
		}
	} else {
		log.Debugf("WORKER_THREADS_NODEVM_ATTACH is not set. Picking the default value %d",
			defaultMaxWorkerThreadsForNodeVMAttach)
	}
	return workerThreads
}

// recordEvent records the event, sets the backOffDuration for the instance
// appropriately and logs the message.
// backOffDuration is reset to 1 second on success and doubled on failure.
func recordEvent(ctx context.Context, r *ReconcileCnsNodeVMAttachment,
	instance *cnsnodevmattachmentv1alpha1.CnsNodeVmAttachment, eventtype string, msg string) {
	log := logger.GetLogger(ctx)
	namespacedName := k8stypes.NamespacedName{
		Name:      instance.Name,
		Namespace: instance.Namespace,
	}
	switch eventtype {
	case v1.EventTypeWarning:
		// Double backOff duration.
		backOffDurationMapMutex.Lock()
		backOffDuration[namespacedName] = backOffDuration[namespacedName] * 2
		backOffDurationMapMutex.Unlock()
		r.recorder.Event(instance, v1.EventTypeWarning, "NodeVMAttachFailed", msg)
		log.Error(msg)
	case v1.EventTypeNormal:
		// Reset backOff duration to one second.
		backOffDurationMapMutex.Lock()
		backOffDuration[namespacedName] = time.Second
		backOffDurationMapMutex.Unlock()
		r.recorder.Event(instance, v1.EventTypeNormal, "NodeVMAttachSucceeded", msg)
		log.Info(msg)
	}
}

func addCNSFinalizer(ctx context.Context, c client.Client,
	instance *cnsnodevmattachmentv1alpha1.CnsNodeVmAttachment) error {
	// TODO: we can use the AddFinalizer function from the k8s library
	for _, finalizer := range instance.Finalizers {
		if finalizer == cnsoperatortypes.CNSFinalizer {
			// already exists. No patch needed.
			return nil
		}
	}

	return k8s.PatchFinalizers(ctx, c, instance, append(instance.Finalizers, cnsoperatortypes.CNSFinalizer))
}
