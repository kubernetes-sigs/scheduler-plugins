package deadline

import (
	"testing"
	"time"

	gocache "github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/suite"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/annotations"
)

type DDLManagerSuite struct {
	suite.Suite
	deadlineCache *gocache.Cache
	existingPod   *v1.Pod
}

func TestDDLManagerSuite(t *testing.T) {
	suite.Run(t, new(DDLManagerSuite))
}

func (suite *DDLManagerSuite) SetupSuite() {
	suite.deadlineCache = gocache.New(time.Second, time.Second)
}

func (suite *DDLManagerSuite) SetupTest() {
	existingPodUID := "pod-existing"
	suite.existingPod = st.MakePod().UID(existingPodUID).Annotations(map[string]string{annotations.AnnotationKeyDDL: "10s"}).CreationTimestamp(metav1.NewTime(time.Now())).Obj()
	suite.deadlineCache.Add(existingPodUID, suite.existingPod, time.Second*2)
}

func (suite *DDLManagerSuite) TearDownTest() {
	for k := range suite.deadlineCache.Items() {
		suite.deadlineCache.Delete(k)
	}
}

func (suite *DDLManagerSuite) TestParsePodDeadline() {
	manager := deadlineManager{podDeadlines: suite.deadlineCache}
	now := time.Now()
	pod := st.MakePod().Annotations(map[string]string{annotations.AnnotationKeyDDL: "10s"}).CreationTimestamp(metav1.NewTime(now)).Obj()
	suite.Equal(now.Add(10*time.Second), manager.ParsePodDeadline(pod))
}

func (suite *DDLManagerSuite) TestAddPodDeadline() {
	manager := deadlineManager{podDeadlines: suite.deadlineCache}
	now := time.Now()
	pod := st.MakePod().Annotations(map[string]string{annotations.AnnotationKeyDDL: "20s"}).CreationTimestamp(metav1.NewTime(now)).Obj()
	suite.Equal(now.Add(20*time.Second), manager.AddPodDeadline(pod))
}

func (suite *DDLManagerSuite) TestRemovePodDeadline() {
	manager := deadlineManager{podDeadlines: suite.deadlineCache}
	manager.RemovePodDeadline(suite.existingPod)
	suite.Len(suite.deadlineCache.Items(), 0)
}

func (suite *DDLManagerSuite) TestGetPodDeadline() {
	manager := deadlineManager{podDeadlines: suite.deadlineCache}
	now := time.Now()
	pod := st.MakePod().Annotations(map[string]string{annotations.AnnotationKeyDDL: "30s"}).CreationTimestamp(metav1.NewTime(now)).Obj()
	suite.Equal(now.Add(time.Second*30), manager.GetPodDeadline(pod))
}
