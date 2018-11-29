/*
Copyright 2018 The Kubernetes authors.

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

package mutating

import (
	"context"
	"fmt"
	"net/http"

	volumesnapshotv1alpha1 "github.com/kubernetes-csi/external-snapshotter/pkg/apis/volumesnapshot/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/runtime/inject"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission/types"
)

var supportedSourceKinds = sets.NewString(string("PersistentVolumeClaim"))
var supportedDataSourceAPIGroups = sets.NewString(string(""))

func init() {
	webhookName := "mutating-create-update-volumesnapshots"
	if HandlerMap[webhookName] == nil {
		HandlerMap[webhookName] = []admission.Handler{}
	}
	HandlerMap[webhookName] = append(HandlerMap[webhookName], &VolumeSnapshotCreateUpdateHandler{})
}

// VolumeSnapshotCreateUpdateHandler handles VolumeSnapshot
type VolumeSnapshotCreateUpdateHandler struct {
	Client client.Client

	// Decoder decodes objects
	Decoder types.Decoder
}

// validateSnapshot checks whether the values set on the VolumeSnapshot object are valid.
// also set the snapshot class if it is not specified
func (h *VolumeSnapshotCreateUpdateHandler) validateSnapshotSource(ctx context.Context, snapshot *volumesnapshotv1alpha1.VolumeSnapshot) (string, error) {
	// TODO(user): implement your admission logic

	//var pvc corev1.PVC
	//err := h.Client.Get(ctx, client.ObjectKey(apitypes.NamespacedName{Namespace: "", Name: ""}), &pvc)

	if snapshot.Spec.Source != nil {
		source := snapshot.Spec.Source
		if len(source.Name) == 0 {
			return "", fmt.Errorf("Snapshot.Spec.Source.Name can not be empty")
		} else if !supportedSourceKinds.Has(string(source.Kind)) {
			return "", fmt.Errorf("Snapshot.Spec.Source.Kind exepct %v, got %q", supportedSourceKinds, source.Kind)
		}
		pvcName := snapshot.Spec.Source.Name
		if pvcName == "" {
			return "", fmt.Errorf("the PVC name is not specified in snapshot %s/%s", snapshot.Namespace, snapshot.Name)
		}
		var pvc corev1.PersistentVolumeClaim
		err := h.Client.Get(ctx, client.ObjectKey(apitypes.NamespacedName{Namespace: snapshot.Namespace, Name: pvcName}), &pvc)
		if err != nil {
			return "", fmt.Errorf("Fail to get PVC")
		}

		if pvc.Status.Phase != corev1.ClaimBound {
			return "", fmt.Errorf("the PVC %s is not yet bound to a PV, will not attempt to take a snapshot", pvc.Name)
		}
		storageClassName := pvc.Spec.StorageClassName
		if storageClassName == nil || len(*storageClassName) == 0 {
			return "", fmt.Errorf("fail to get storage class from the pvc source")
		}

		var sc corev1.StorageClass
		err = h.Client.Get(ctx, client.ObjectKey(apitypes.NamespacedName{Name: storageClassName}), &sc)
		if err != nil {
			return "", fmt.Errorf("Fail to get storageclass")
		}
		return sc.Provisioner, nil
	}
	// if source is not specified, the content name must be set
	if snapshot.Spec.SnapshotContentName == "" {
		return "", fmt.Errorf("Cannot set Snapshot.Spec.Source to nil and snapshot.Spec.SnapshotContentName to empty at the same time.")
	}
	if snapshot.Spec.VolumeSnapshotClassName == nil {
		return "", fmt.Errorf("Snapshot class must be set in static binding senario")
	}

	var snapshotContent volumesnapshotv1alpha1.VolumeSnapshotContent
	err := h.Client.Get(ctx, client.ObjectKey(apitypes.NamespacedName{Name: snapshot.Spec.SnapshotContentName}), &snapshotContent)
	if err != nil {
		return "", fmt.Errorf("Fail to get snapshot content")
	}
	if *snapshot.Spec.VolumeSnapshotClassName != *snapshotContent.Spec.VolumeSnapshotClassName {
		return "", fmt.Errorf("snapshot class name does not match")
	}
	return "", nil
}

// IsDefaultAnnotation returns a boolean if
// the annotation is set
func IsDefaultAnnotation(obj metav1.ObjectMeta) bool {
	if obj.Annotations[IsDefaultSnapshotClassAnnotation] == "true" {
		return true
	}

	return false
}

func (h *VolumeSnapshotCreateUpdateHandler) getDefaultSnapshotClassName(ctx context.Context, driver string) (string, error) {
	var list volumesnapshotv1alpha1.VolumeSnapshotClassList
	err := h.Client.List(ctx, &client.ListOptions{}, &list)
	if err != nil {
		return "", fmt.Errorf("fail to get snapshot class list")
	}

	defaultClasses := []volumesnapshotv1alpha1.VolumeSnapshotClass{}

	for _, class := range list.Items {
		if IsDefaultAnnotation(class.ObjectMeta) && class.Snapshotter == driver {
			defaultClasses = append(defaultClasses, class)
			glog.V(5).Infof("get defaultClass added: %s", class.Name)
		}
	}
	if len(defaultClasses) == 0 {
		return "", fmt.Errorf("cannot find default snapshot class")
	}
	if len(defaultClasses) > 1 {
		glog.V(4).Infof("get DefaultClass %d defaults found", len(defaultClasses))
		return "", fmt.Errorf("%d default snapshot classes were found", len(defaultClasses))
	}
	return defaultClasses[0].Name, nil
}

func (h *VolumeSnapshotCreateUpdateHandler) validateandDefaultingSnapshotClass(ctx context.Context, snapshot *volumesnapshotv1alpha1.VolumeSnapshot, provisioner string) error {
	snapshotClassName := snapshot.Spec.VolumeSnapshotClassName
	if snapshotClassName != nil || len(*snapshotClassName) > 0 {
		var snapshotclass volumesnapshotv1alpha1.VolumeSnapshotClassList
		err := h.Client.Get(ctx, client.ObjectKey(apitypes.NamespacedName{Name: snapshotClassName}), &snapshotclass)
		if err != nil {
			return err
		}
		if snapshotclass.Spec.snapshotter != provisioner {
			return fmt.Errorf("the snapshotter does not match provisioner")
		}
	}

	// get default snapshot class
	defaultClassName, err := getDefaultSnapshotClassName(ctx, provisioner)
	if err != nil {
		return err
	}
	snapshot.Spec.VolumeSnapshotClassName = &defaultClassName
	return nil
}

func (h *VolumeSnapshotCreateUpdateHandler) mutatingVolumeSnapshotFn(ctx context.Context, snapshot *volumesnapshotv1alpha1.VolumeSnapshot) error {
	// TODO(user): implement your admission logic
	var snapshotClass volumesnapshotv1alpha1.VolumeSnapshotClassList
	h.Client.List(ctx, &client.ListOptions{}, &snapshotClass)
	//var pvc corev1.PVC
	//err := h.Client.Get(ctx, client.ObjectKey(apitypes.NamespacedName{Namespace: "", Name: ""}), &pvc)

	return nil
}

var _ admission.Handler = &VolumeSnapshotCreateUpdateHandler{}

// Handle handles admission requests.
func (h *VolumeSnapshotCreateUpdateHandler) Handle(ctx context.Context, req types.Request) types.Response {
	obj := &volumesnapshotv1alpha1.VolumeSnapshot{}

	err := h.Decoder.Decode(req, obj)
	if err != nil {
		return admission.ErrorResponse(http.StatusBadRequest, err)
	}
	copy := obj.DeepCopy()

	provisioner, err = h.validateSnapshotSource(ctx, copy)
	if err != nil {
		return admission.ErrorResponse(http.StatusInternalServerError, err)
	}
	err = h.validateandDefaultingSnapshotClass(ctx, copy, provisioner)
	if err != nil {
		return admission.ErrorResponse(http.StatusInternalServerError, err)
	}
	return admission.PatchResponse(obj, copy)
}

var _ inject.Client = &VolumeSnapshotCreateUpdateHandler{}

// InjectClient injects the client into the VolumeSnapshotCreateUpdateHandler
func (h *VolumeSnapshotCreateUpdateHandler) InjectClient(c client.Client) error {
	h.Client = c
	return nil
}

var _ inject.Decoder = &VolumeSnapshotCreateUpdateHandler{}

// InjectDecoder injects the decoder into the VolumeSnapshotCreateUpdateHandler
func (h *VolumeSnapshotCreateUpdateHandler) InjectDecoder(d types.Decoder) error {
	h.Decoder = d
	return nil
}
