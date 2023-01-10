/*
Copyright 2017 The Kubernetes Authors.

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

package nfsexport

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"encoding/json"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/volume"
	mount "k8s.io/mount-utils"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	//"github.com/hexops/valast"
)

// NodeServer driver
type NodeServer struct {
	Driver  *Driver
	mounter mount.Interface
	localPath string
	exportPath string
}

// NodePublishVolume mount the volume
func (ns *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	targetPath := req.GetTargetPath()
	klog.V(2).Infof("Target Path is : %s", targetPath)
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	mountOptions := volCap.GetMount().GetMountFlags()
	if req.GetReadonly() {
		mountOptions = append(mountOptions, "ro")
	}

	mountPermissions := ns.Driver.mountPermissions
	var appPodName, appPodNs string
	for k, v := range req.GetVolumeContext() {
		switch strings.ToLower(k) {
		case podNameKey:
			appPodName = v
		case podNamespaceKey:
			appPodNs = v
		case mountOptionsField:
			if v != "" {
				mountOptions = append(mountOptions, v)
			}
		case mountPermissionsField:
			if v != "" {
				var err error
				if mountPermissions, err = strconv.ParseUint(v, 8, 32); err != nil {
					return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("invalid mountPermissions %s", v))
				}
			}
		}
	}
	klog.V(2).Infof("Applicaion Pod Name is: %s", appPodName)
	klog.V(2).Infof("Application Pod Namespace is: %s", appPodNs)

	// Mount nfs export path for local path
	notMnt, err := ns.mounter.IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(targetPath, os.FileMode(mountPermissions)); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			notMnt = true
		} else {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if !notMnt {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// Mount local first, if fails, then mount remote
	source := ns.localPath
	err = ns.mounter.Mount(source, targetPath, "", []string{"bind"})
	if err == nil {
		appPod, err := ns.Driver.clientSet.CoreV1().Pods(appPodNs).Get(context.TODO(), appPodName, metav1.GetOptions{})
		annotations := appPod.ObjectMeta.Annotations
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["controller.kubernetes.io/pod-deletion-cost"] = "2147483647"
		appPod.SetAnnotations(annotations)
		if err != nil {
			klog.V(2).Infof("Failed to get the application pod: %s", err)
		} else {
			_, err = ns.Driver.clientSet.CoreV1().Pods(appPodNs).Update(context.TODO(), appPod, metav1.UpdateOptions{})
			if err != nil {
				klog.V(2).Infof("Failed to annotate the application pod: %s", err)
			} else {
				klog.V(2).Infof("Annotated pod with the highest deletion-cost: %s", appPodName )
			}
		}
	} else {
		source = ns.exportPath
		err = ns.mounter.Mount(source, targetPath, "nfs", mountOptions)
	}


	klog.V(2).Infof("NodePublishVolume: volumeID(%v) source(%s) targetPath(%s) mountflags(%v)", volumeID, source, targetPath, mountOptions)

	if err != nil {
		if os.IsPermission(err) {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		if strings.Contains(err.Error(), "invalid argument") {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	if mountPermissions > 0 {
		if err := chmodIfPermissionMismatch(targetPath, os.FileMode(mountPermissions)); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	} else {
		klog.V(2).Infof("skip chmod on targetPath(%s) since mountPermissions is set as 0", targetPath)
	}
	
	klog.V(2).Infof("volume(%s) mount %s on %s succeeded", volumeID, source, targetPath)
	
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmount the volume
func (ns *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	targetPath := req.GetTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	klog.V(2).Infof("NodeUnpublishVolume: unmounting volume %s on %s", volumeID, targetPath)
	err := mount.CleanupMountPoint(targetPath, ns.mounter, true /*extensiveMountPointCheck*/)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unmount target %q: %v", targetPath, err)
	}
	klog.V(2).Infof("NodeUnpublishVolume: unmount volume %s on %s successfully", volumeID, targetPath)

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeStageVolume stage volume
func (ns *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	// Get parameters
	var backendPvcName, backendNs, backendImg, hostIPs string
	bs, _ := json.Marshal(req.GetVolumeContext())
    klog.V(2).Infof("VolumeContext: %s", string(bs))
	for k, v := range req.GetVolumeContext() {
		switch strings.ToLower(k) {
		case paramBackendVolumeClaim:
			backendPvcName = v
		case paramBackendNamespace:
			backendNs = v
		case paramBackendPodImage:
			backendImg = v
		case paramHostIPs:
			hostIPs = v
		}
	}
	if backendPvcName == "" {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("%v is a required parameter", paramBackendVolumeClaim))
	}
	if backendNs == "" {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("%v is a required parameter", paramBackendNamespace))
	}
	if backendImg == "" {
		backendImg = "daocloud.io/piraeus/nfs-ganesha:latest"
	}

	// Check if backend PVC exists
	backendPvc, err := ns.Driver.clientSet.CoreV1().PersistentVolumeClaims(backendNs).Get(context.TODO(), backendPvcName, metav1.GetOptions{})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	backendPvcUid := backendPvc.ObjectMeta.UID

	// Create backend Pod to connect backend SVC with backend PVC
	backendStsName := backendPvcName
	klog.Infof("Creating backend StatefulSet is: \"%s\"", backendStsName)
	backendSvcName := backendPvcName
	
	hostPathType := corev1.HostPathDirectoryOrCreate
	mountPropagationMode := corev1.MountPropagationBidirectional
	backendStsDef := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: backendStsName,
			Labels: map[string]string{
				"nfs-export.csi.k8s.io/id": volumeID,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "PersistentVolumeClaim",
					Name:               backendPvcName,
					UID:                backendPvcUid,
				},
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: Ptr(int32(1)),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.OnDeleteStatefulSetStrategyType,	
			},
			ServiceName: backendSvcName,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"nfs-export.csi.k8s.io/id": volumeID,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"nfs-export.csi.k8s.io/id": volumeID,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyAlways,
					TerminationGracePeriodSeconds: Ptr(int64(30)),
					Containers: []corev1.Container{
						{
							Name:  "export",
							Image: backendImg,
							ImagePullPolicy: corev1.PullIfNotPresent,
							SecurityContext: &corev1.SecurityContext{
								Privileged: Ptr(true), //valast.Addr(true).(*bool),
							},
							// SecurityContext: &corev1.SecurityContext{
							// 	Capabilities: &corev1.Capabilities{
							// 		Add: []corev1.Capability{
							// 			"SYS_ADMIN",
							// 			"SETPCAP",
							// 			"DAC_READ_SEARCH",
							// 		},
							// 	},
							// },
							Args: []string{
								hostIPs,
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "nfs",
									Protocol:      corev1.ProtocolTCP,
									ContainerPort: 2049,
								},
								{
									Name:          "rpc-tcp",
									Protocol:      corev1.ProtocolTCP,
									ContainerPort: 111,
								},
								{
									Name:          "rpc-udp",
									Protocol:      corev1.ProtocolUDP,
									ContainerPort: 111,
								},
							},
							ReadinessProbe: &corev1.Probe {
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
											Port: intstr.FromString("nfs"),
									},
								},
								InitialDelaySeconds: 1,
								PeriodSeconds: 1,
								SuccessThreshold: 3,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:			  "volumes",
									MountPath:		  "/volumes",
									MountPropagation: &mountPropagationMode,
								},
								{
									Name:	   "data",
									MountPath: "/volumes/" + volumeID,
								},
								{
									Name:	   "data",
									MountPath: "/export",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "volumes", 
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/csi-nfs-export/volumes/",
									Type: &hostPathType,
								},
							},
						},
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: backendPvcName,
								},
							},
						},
					},
				},
			},
		},
	}

	_, err = ns.Driver.clientSet.AppsV1().StatefulSets(backendNs).Get(context.TODO(), backendStsName, metav1.GetOptions{})
	if err != nil {
		_, err = ns.Driver.clientSet.AppsV1().StatefulSets(backendNs).Create(context.TODO(), backendStsDef, metav1.CreateOptions{})
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}

	// Wait for Pod to be ready
	backendPodName := backendStsName + "-0"
	// backendPod, _ := ns.Driver.clientSet.CoreV1().Pods(backendNs).Get(context.TODO(), backendPodName, metav1.GetOptions{})
	// backendPodUid:= backendPod.ObjectMeta.UID
	// klog.V(2).Infof("Backend Pod UID is: \"%s\"", backendPodUid )

	klog.V(2).Infof("Waiting for Pod to be ready: %s", backendPodName)
	err = waitForPodRunning(ns.Driver.clientSet, backendNs, backendPodName, 5 * time.Minute)
	if err != nil {
		klog.V(2).Infof("Pod wait has timed out: %s", backendPodName)
		return nil, status.Error(codes.Canceled, err.Error())
	}

	// Create backend Service
	klog.V(2).Infof("Creating backend Service: \"%s\"", backendSvcName )
	backendSvcDef := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: backendSvcName,
			Labels: map[string]string{
				"nfs-export.csi.k8s.io/id": volumeID,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "PersistentVolumeClaim",
					Name:               backendPvcName,
					UID:                backendPvcUid,
				},
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
					"nfs-export.csi.k8s.io/id": volumeID,
				},
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:		"nfs",
					Protocol:	corev1.ProtocolTCP,
					Port:		2049,
				},
				{
					Name:		"rpc-tcp",
					Protocol:	corev1.ProtocolTCP,
					Port:		111,
				},
				{
					Name:		"rpc-udp",
					Protocol:	corev1.ProtocolUDP,
					Port:		111,
				},
			},
		},
	}

	_, err = ns.Driver.clientSet.CoreV1().Services(backendNs).Get(context.TODO(), backendSvcName, metav1.GetOptions{})
	if err != nil {
		_, err := ns.Driver.clientSet.CoreV1().Services(backendNs).Create(context.TODO(), backendSvcDef, metav1.CreateOptions{})
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}

	var backendClusterIp string
	backendSvc, err := ns.Driver.clientSet.CoreV1().Services(backendNs).Get(context.TODO(), backendSvcName, metav1.GetOptions{})
	if err == nil {
		backendClusterIp = backendSvc.Spec.ClusterIP;
		klog.V(2).Infof("Backend IP is \"%s\"", backendClusterIp)
	} else {
		klog.V(2).Infof("Failed to create service: \"%s\"", backendSvcName)
		return nil, status.Error(codes.Canceled, err.Error())
	}

	// Wait for NFS to be online
	klog.V(2).Infof("Waiting for NFS TCP to be ok: %s:2049", backendClusterIp)
	err = waitForTcpReady(backendClusterIp, 2049, time.Minute)
	if err != nil {
		klog.V(2).Infof("TCP wait has timed out: %s:2049", backendClusterIp)
		return nil, status.Error(codes.Canceled, err.Error())
	}
	
	ns.exportPath = backendClusterIp + ":" + "/"
	// ns.localPath = fmt.Sprintf("/var/snap/microk8s/common/var/lib/kubelet/pods/%s/volumes/kubernetes.io~csi/pvc-%s/mount", backendPodUid, backendPvcUid )
	// ns.localPath = fmt.Sprintf("/pods/%s/volumes/kubernetes.io~csi/pvc-%s/mount", backendPodUid, backendPvcUid )
	ns.localPath = "/volumes/" + volumeID

	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume unstage volume
func (ns *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	frontendPv, err := getPvById(ns.Driver.clientSet, volumeID)
	if err != nil {
		klog.V(2).Infof("Cannot find frontend PV by ID: %s", volumeID )
		return &csi.NodeUnstageVolumeResponse{}, nil
	} 

	// check if there is still any application POD using the frontend PVC 
	frontendPvcName :=  frontendPv.Spec.ClaimRef.Name
	frontendPvcNs :=  frontendPv.Spec.ClaimRef.Namespace
	klog.V(2).Infof("Frontend PVC Namespace is: %s", frontendPvcNs )
	klog.V(2).Infof("Frontend PVC Name is: %s", frontendPvcName)
	podList, _ := ns.Driver.clientSet.CoreV1().Pods(frontendPvcNs).List(context.TODO(), metav1.ListOptions{})
	for _, pod := range podList.Items {
			for _, volume := range pod.Spec.Volumes {
					if volume.PersistentVolumeClaim !=nil && volume.PersistentVolumeClaim.ClaimName == frontendPvcName {
						klog.V(2).Infof("Pod %s is still using the PV", pod.ObjectMeta.Name )
						return &csi.NodeUnstageVolumeResponse{}, nil
					}
			}
	}

	// Delete backend pod and svc
	backendNs := frontendPv.Spec.PersistentVolumeSource.CSI.VolumeAttributes["backendNamespace"]
	backendPvcName := frontendPv.Spec.PersistentVolumeSource.CSI.VolumeAttributes["backendVolumeClaim"]
	backendStsName := backendPvcName
	backendSvcName := backendPvcName
	klog.V(2).Infof("Backend PVC Namespace is: %s", backendNs )
	klog.V(2).Infof("Backend StatefulSet Name is: %s", backendStsName)
	klog.V(2).Infof("Backend Service Name is: %s", backendSvcName)

	err = ns.Driver.clientSet.AppsV1().StatefulSets(backendNs).Delete(context.TODO(), backendStsName, metav1.DeleteOptions{})
	if err == nil {
		klog.V(2).Infof("Deleted backend STS %s", backendStsName)
	} else {
		klog.V(2).Infof("Failed to delete backend STS %s: %s", backendStsName, err)
	}

	err = ns.Driver.clientSet.CoreV1().Services(backendNs).Delete(context.TODO(), backendSvcName, metav1.DeleteOptions{})
	if err == nil {
		klog.V(2).Infof("Deleted backend SVC %s", backendSvcName)
	} else {
		klog.V(2).Infof("Failed to delete backend SVC %s: %s", backendSvcName, err)
	}

	err = os.Remove(ns.localPath)
	if err == nil {
		klog.V(2).Infof("Removed the local path %s", "/var/lib/csi-nfs-export/" + ns.localPath)
	} else { 
		klog.V(2).Infof("Failed to removed the local path %s: %s", "/var/lib/csi-nfs-export/" + ns.localPath, err)
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodeExpandVolume node expand volume
func (ns *NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// NodeGetVolumeStats get volume stats
func (ns *NodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if len(req.VolumeId) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats volume ID was empty")
	}
	if len(req.VolumePath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats volume path was empty")
	}

	if _, err := os.Lstat(req.VolumePath); err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "path %s does not exist", req.VolumePath)
		}
		return nil, status.Errorf(codes.Internal, "failed to stat file %s: %v", req.VolumePath, err)
	}

	volumeMetrics, err := volume.NewMetricsStatFS(req.VolumePath).GetMetrics()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get metrics: %v", err)
	}

	available, ok := volumeMetrics.Available.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform volume available size(%v)", volumeMetrics.Available)
	}
	capacity, ok := volumeMetrics.Capacity.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform volume capacity size(%v)", volumeMetrics.Capacity)
	}
	used, ok := volumeMetrics.Used.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform volume used size(%v)", volumeMetrics.Used)
	}

	inodesFree, ok := volumeMetrics.InodesFree.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform disk inodes free(%v)", volumeMetrics.InodesFree)
	}
	inodes, ok := volumeMetrics.Inodes.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform disk inodes(%v)", volumeMetrics.Inodes)
	}
	inodesUsed, ok := volumeMetrics.InodesUsed.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform disk inodes used(%v)", volumeMetrics.InodesUsed)
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Unit:      csi.VolumeUsage_BYTES,
				Available: available,
				Total:     capacity,
				Used:      used,
			},
			{
				Unit:      csi.VolumeUsage_INODES,
				Available: inodesFree,
				Total:     inodes,
				Used:      inodesUsed,
			},
		},
	}, nil
}

// NodeGetInfo return info of the node on which this plugin is running
func (ns *NodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: ns.Driver.nodeID,
	}, nil
}

// NodeGetCapabilities return the capabilities of the Node plugin
func (ns *NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: ns.Driver.nscap,
	}, nil
}