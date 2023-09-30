package annotations

const (
	// AnnotationKeyPrefix is the prefix of the annotation key
	AnnotationKeyPrefix = "rt-preemptive.scheduling.x-k8s.io/"
	// AnnotationKeyDDL represents the relative deadline of a program
	AnnotationKeyDDL = AnnotationKeyPrefix + "ddl"
	// AnnotationKeyPausePod represents whether or not a pod is marked to be paused
	AnnotationKeyPausePod = AnnotationKeyPrefix + "pause-pod"
)
