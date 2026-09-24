/*
Copyright 2026 The Beskar7 Authors.

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

package v1beta2

import (
	"fmt"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

// TestDeepCopySharesNoMemory fills every exported field of every kind this
// package registers (every pointer set, every slice and map non-empty) and
// requires DeepCopyObject to return an equal object that shares no pointer,
// slice or map with the original. A shared one means the controller-runtime
// cache hands a reconciler memory that another reconcile, or the cache itself,
// still holds: mutating the copy mutates the original.
//
// controller-gen skips any type that already defines DeepCopyInto, so a
// hand-written one is never regenerated when its type gains a field. Through
// v0.8.0 two had fallen behind: InspectionReport shared each NIC's IPAddresses,
// and Beskar7ClusterStatus shared each failure domain's ControlPlane and
// Attributes.
func TestDeepCopySharesNoMemory(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kinds := scheme.KnownTypes(GroupVersion)
	if len(kinds) == 0 {
		t.Fatal("no kinds registered for " + GroupVersion.String())
	}
	for kind, typ := range kinds {
		if typ.PkgPath() != reflect.TypeFor[PhysicalHost]().PkgPath() {
			continue // WatchEvent, ListOptions and the like, registered by metav1
		}
		t.Run(kind, func(t *testing.T) {
			original := reflect.New(typ)
			fillAll(original.Elem(), 0)
			obj := original.Interface().(runtime.Object)

			copied := obj.DeepCopyObject()
			if !reflect.DeepEqual(obj, copied) {
				t.Fatalf("DeepCopyObject returned an object that differs from the original")
			}
			for _, path := range sharedMemory(reflect.ValueOf(obj).Elem(), reflect.ValueOf(copied).Elem(), kind) {
				t.Errorf("%s is shared between the original and its deep copy", path)
			}
		})
	}
}

// fillAll sets every exported field reachable from v to a non-zero value.
// Unexported fields are left alone: they belong to types such as time.Time and
// resource.Quantity, whose own DeepCopy is not in question here.
func fillAll(v reflect.Value, depth int) {
	if depth > 12 {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillAll(v.Elem(), depth+1)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fillAll(v.Field(i), depth+1)
			}
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		for i := range v.Len() {
			fillAll(v.Index(i), depth+1)
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		for i := range 2 {
			key := reflect.New(v.Type().Key()).Elem()
			if key.Kind() == reflect.String {
				key.SetString(fmt.Sprintf("key-%d", i))
			} else {
				fillAll(key, depth+1)
			}
			value := reflect.New(v.Type().Elem()).Elem()
			fillAll(value, depth+1)
			v.SetMapIndex(key, value)
		}
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	}
}

// sharedMemory walks a and b in step and returns the path of every pointer,
// slice or map they share.
func sharedMemory(a, b reflect.Value, path string) []string {
	var shared []string
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return nil
		}
		if a.Pointer() == b.Pointer() {
			return []string{path}
		}
		return sharedMemory(a.Elem(), b.Elem(), path)
	case reflect.Struct:
		for i := range a.NumField() {
			if f := a.Type().Field(i); f.IsExported() {
				shared = append(shared, sharedMemory(a.Field(i), b.Field(i), path+"."+f.Name)...)
			}
		}
	case reflect.Slice:
		if a.Len() == 0 || b.Len() != a.Len() {
			return nil
		}
		if a.Pointer() == b.Pointer() {
			return []string{path}
		}
		for i := range a.Len() {
			shared = append(shared, sharedMemory(a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", path, i))...)
		}
	case reflect.Map:
		if a.Len() == 0 || b.IsNil() {
			return nil
		}
		if a.Pointer() == b.Pointer() {
			return []string{path}
		}
		for _, key := range a.MapKeys() {
			shared = append(shared, sharedMemory(a.MapIndex(key), b.MapIndex(key), fmt.Sprintf("%s[%v]", path, key))...)
		}
	}
	return shared
}
