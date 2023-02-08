package v1alpha1

import (
    //"sigs.k8s.io/security-profiles-operator/api/seccompprofile/v1beta1"
    "k8s.io/apimachinery/pkg/runtime/schema"
    //"k8s.io/apimachinery/pkg/runtime/serializer"
    "k8s.io/client-go/kubernetes/scheme"
    "k8s.io/client-go/rest"
)

type SPOV1Alpha1Interface interface {
    //Profiles(namespace string) ProfileInterface 
    Profiles() ProfileInterface 
}

type SPOV1Alpha1Client struct {
    restClient rest.Interface
}

func NewForConfig(c *rest.Config) (*SPOV1Alpha1Client, error) {
    config := *c
    config.GroupVersion = &schema.GroupVersion{Group: "security-profiles-operator.x-k8s.io", Version: "v1beta1"}
    config.APIPath = "/apis"
    //crdConfig.NegotiatedSerializer = serializer.NewCodecFactory(scheme.Scheme)
    config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
    //crdConfig.UserAgent = rest.DefaultKubernetesUserAgent()
    if config.UserAgent == "" {
        config.UserAgent = rest.DefaultKubernetesUserAgent()
    }

    client, err := rest.RESTClientFor(&config)
    if err != nil {
        return nil, err
    }

    return &SPOV1Alpha1Client{restClient: client}, nil
}

//func (c *SPOV1Alpha1Client) Profiles(namespace string) ProfileInterface {
func (c *SPOV1Alpha1Client) Profiles() ProfileInterface {
    return &profileClient{
        restClient: c.restClient,
        //ns: namespace,
    }
}

