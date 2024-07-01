package resource

import (
	"reflect"
	"testing"
)

func TestResourceCache_SetExtendedResource(t *testing.T) {
	type args struct {
		nodeName string
		val      ExtendedResource
	}
	tests := []struct {
		name string
		args args
	}{
		{
			name: "Set EC",
			args: args{
				nodeName: "testNode",
				val:      &FakeResource{},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := NewExtendedCache()
			cache.SetExtendedResource(tt.args.nodeName, tt.args.val)
			if cache.GetExtendedResource(tt.args.nodeName) == nil {
				t.Errorf("SetExtendedResource() failed")
			}
		})
	}
}

func TestResourceCache_GetExtendedResource(t *testing.T) {
	type args struct {
		nodeName string
	}
	tests := []struct {
		name string
		args args
		want ExtendedResource
	}{
		{
			name: "Get EC",
			args: args{
				nodeName: "testNode",
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := NewExtendedCache()
			if got := cache.GetExtendedResource(tt.args.nodeName); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ResourceCache.GetExtendedResource() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResourceCache_DeleteExtendedResource(t *testing.T) {
	type args struct {
		nodeName string
	}
	tests := []struct {
		name string
		args args
	}{
		{
			name: "Delete EC",
			args: args{
				nodeName: "testNode",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := NewExtendedCache()
			cache.SetExtendedResource(tt.args.nodeName, &FakeResource{})
			cache.DeleteExtendedResource(tt.args.nodeName)
			if cache.GetExtendedResource(tt.args.nodeName) != nil {
				t.Errorf("DeleteExtendedResource() failed")
			}
		})
	}
}

func TestResourceCache_PrintCacheInfo(t *testing.T) {
	tests := []struct {
		name   string
	}{
		{
			name: "Print EC",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := NewExtendedCache()
			cache.SetExtendedResource("testNode", &FakeResource{})
			cache.PrintCacheInfo()
		})
	}
}