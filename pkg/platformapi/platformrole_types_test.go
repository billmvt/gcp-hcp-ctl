package platformapi

import (
	"encoding/json"
	"reflect"
	"testing"

	gcpv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestPlatformRoleWireTypes checks registration, decoding and round trips for CLI-local
// catalog types.
func TestPlatformRoleWireTypes(t *testing.T) {
	wire := `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"PlatformRole","metadata":{"name":"future-role","creationTimestamp":"2026-01-01T00:00:00Z"},"spec":{"permissions":["cluster.get","rolebinding.list"]}}`
	object, gvk, err := codecs.UniversalDeserializer().Decode([]byte(wire), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	role, ok := object.(*PlatformRole)
	if !ok || *gvk != gcpv1.GroupVersion.WithKind("PlatformRole") {
		t.Fatalf("unexpected resource: %T %v", object, gvk)
	}
	if role.Name != "future-role" || !reflect.DeepEqual(role.Spec.Permissions, []string{"cluster.get", "rolebinding.list"}) {
		t.Fatalf("incomplete wire projection: %#v", role)
	}
	encoded, err := json.Marshal(role)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	if err := json.Unmarshal([]byte(wire), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("wire shape changed: %s", encoded)
	}
	listWire := `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"PlatformRoleList","metadata":{"continue":"next","remainingItemCount":1},"items":[` + wire + `]}`
	object, gvk, err = codecs.UniversalDeserializer().Decode([]byte(listWire), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	list, ok := object.(*PlatformRoleList)
	if !ok || *gvk != gcpv1.GroupVersion.WithKind("PlatformRoleList") || list.Continue != "next" || len(list.Items) != 1 || list.Items[0].Name != role.Name {
		t.Fatalf("unexpected list: %#v %v", object, gvk)
	}
}

// TestPlatformRoleWireDeepCopies checks that metadata, permissions and list items are
// copied independently.
func TestPlatformRoleWireDeepCopies(t *testing.T) {
	role := &PlatformRole{ObjectMeta: metav1.ObjectMeta{Name: "reader", Annotations: map[string]string{"tracking": "original"}}, Spec: PlatformRoleSpec{Permissions: []string{"cluster.get"}}}
	original := role.DeepCopy()
	copy := role.DeepCopyObject().(*PlatformRole)
	copy.Annotations["tracking"] = "changed"
	copy.Spec.Permissions[0] = "changed"
	if !reflect.DeepEqual(role, original) {
		t.Fatal("resource copy aliases original metadata or permissions")
	}
	remaining := int64(1)
	list := &PlatformRoleList{ListMeta: metav1.ListMeta{Continue: "next", RemainingItemCount: &remaining}, Items: []PlatformRole{*role}}
	listCopy := list.DeepCopyObject().(*PlatformRoleList)
	*listCopy.RemainingItemCount = 0
	listCopy.Items[0].Annotations["tracking"] = "list-copy"
	listCopy.Items[0].Spec.Permissions[0] = "list-copy"
	if remaining != 1 || !reflect.DeepEqual(&list.Items[0], original) {
		t.Fatal("list copy aliases original metadata or item data")
	}
	for _, permissions := range [][]string{nil, {}} {
		role.Spec.Permissions = permissions
		if !reflect.DeepEqual(role.DeepCopy().Spec.Permissions, permissions) {
			t.Fatal("copy changed nil/empty permissions")
		}
	}
	for _, items := range [][]PlatformRole{nil, {}} {
		list.Items = items
		if !reflect.DeepEqual(list.DeepCopy().Items, items) {
			t.Fatal("copy changed nil/empty items")
		}
	}
	var nilRole *PlatformRole
	var nilList *PlatformRoleList
	if nilRole.DeepCopy() != nil || nilRole.DeepCopyObject() != nil || nilList.DeepCopy() != nil || nilList.DeepCopyObject() != nil {
		t.Fatal("nil copies must remain nil")
	}
}
