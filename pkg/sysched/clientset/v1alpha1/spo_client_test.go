package v1alpha1

import (
	"testing"
	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/rest"
)

func TestNewForConfig(t *testing.T) {
	config := rest.Config{}

	spoclient, err := NewForConfig(&config)
	assert.Nil(t, err)
	assert.NotNil(t, spoclient)
}

func TestProfiles(t *testing.T) {
	config := rest.Config{}

	spoclient, err := NewForConfig(&config)
        assert.Nil(t, err)
	assert.NotNil(t, spoclient)

	p := spoclient.Profiles()
	assert.NotNil(t, p)
}
