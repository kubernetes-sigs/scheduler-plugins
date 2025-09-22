package mycrossnodepreemption

// ConfigMap document
type ConfigMapDoc struct {
	// Namespace where config map should be saved
	Namespace string
	// Name of the config map
	Name string
	// Label for the config map
	LabelKey string
	// Key of the data
	DataKey string
}
