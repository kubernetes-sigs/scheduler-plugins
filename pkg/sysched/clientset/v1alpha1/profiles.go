package v1alpha1

import (
	"context"
        "sigs.k8s.io/security-profiles-operator/api/seccompprofile/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

type ProfileInterface interface {
	List(opts metav1.ListOptions) (*v1beta1.SeccompProfileList, error)
	Get(name string, ns string, options metav1.GetOptions) (*v1beta1.SeccompProfile, error)
	Create(*v1beta1.SeccompProfile) (*v1beta1.SeccompProfile, error)
	Watch(opts metav1.ListOptions) (watch.Interface, error)
	// ...
}

type profileClient struct {
	restClient rest.Interface
	//ns         string
}

func (c *profileClient) List(opts metav1.ListOptions) (*v1beta1.SeccompProfileList, error) {
	result := v1beta1.SeccompProfileList{}
	err := c.restClient.
		Get().
		//Namespace(c.ns).
		Resource("seccompprofiles").
		VersionedParams(&opts, scheme.ParameterCodec).
		Do(context.TODO()).
		Into(&result)

	return &result, err
}

func (c *profileClient) Get(name string, ns string, opts metav1.GetOptions) (*v1beta1.SeccompProfile, error) {
	result := v1beta1.SeccompProfile{}
	err := c.restClient.
		Get().
		Namespace("default").
		Resource("seccompprofiles").
		Name(name).
		VersionedParams(&opts, scheme.ParameterCodec).
		Do(context.TODO()).
		Into(&result)

	return &result, err
}

func (c *profileClient) Create(profile *v1beta1.SeccompProfile) (*v1beta1.SeccompProfile, error) {
	result := v1beta1.SeccompProfile{}
	err := c.restClient.
		Post().
		//Namespace(c.ns).
		Resource("seccompprofiles").
		Body(profile).
		Do(context.TODO()).
		Into(&result)

	return &result, err
}

func (c *profileClient) Watch(opts metav1.ListOptions) (watch.Interface, error) {
	opts.Watch = true
	return c.restClient.
		Get().
		//Namespace(c.ns).
		Resource("seccompprofiles").
		VersionedParams(&opts, scheme.ParameterCodec).
		Watch(context.Background())
}
