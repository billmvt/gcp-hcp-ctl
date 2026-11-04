package platformapi

import (
	gcpv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PlatformRole is the public, platform-scoped catalog wire type. Keep this small
// read-only projection local so discovery does not require upgrading the shared
// Gecko dependency and migrating unrelated APIs. Role definitions come from the
// server; the CLI does not supply permission bundles.
type PlatformRole struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PlatformRoleSpec `json:"spec"`
}

// PlatformRoleSpec contains the complete permission set returned by the server.
type PlatformRoleSpec struct {
	Permissions []string `json:"permissions"`
}

// PlatformRoleList is the Kubernetes-shaped public catalog response.
type PlatformRoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PlatformRole `json:"items"`
}

// init registers the CLI-local catalog wire types with the shared Platform API serializer.
func init() {
	scheme.AddKnownTypes(gcpv1.GroupVersion, &PlatformRole{}, &PlatformRoleList{})
}

// DeepCopy returns an independent copy of the resource for output preparation.
func (in *PlatformRole) DeepCopy() *PlatformRole {
	if in == nil {
		return nil
	}
	out := *in
	out.ObjectMeta = *in.ObjectMeta.DeepCopy()
	if in.Spec.Permissions != nil {
		out.Spec.Permissions = append([]string{}, in.Spec.Permissions...)
	}
	return &out
}

// DeepCopyObject implements runtime.Object for the REST serializer.
func (in *PlatformRole) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

// DeepCopy returns an independent copy of the list and all its items.
func (in *PlatformRoleList) DeepCopy() *PlatformRoleList {
	if in == nil {
		return nil
	}
	out := *in
	out.ListMeta = *in.ListMeta.DeepCopy()
	if in.Items != nil {
		out.Items = make([]PlatformRole, len(in.Items))
		for i := range in.Items {
			out.Items[i] = *in.Items[i].DeepCopy()
		}
	}
	return &out
}

// DeepCopyObject implements runtime.Object for the REST serializer.
func (in *PlatformRoleList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}
