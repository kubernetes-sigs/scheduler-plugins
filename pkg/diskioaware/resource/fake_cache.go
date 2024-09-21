package resource

type FakeCache struct {
	SetCacheFunc    func(string, ExtendedResource)
	GetCacheFunc    func(string) ExtendedResource
	DeleteCacheFunc func(string)
}

func (cache *FakeCache) SetExtendedResource(nodeName string, val ExtendedResource) {
	cache.SetCacheFunc(nodeName, val)
}

func (cache *FakeCache) GetExtendedResource(nodeName string) ExtendedResource {
	return cache.GetCacheFunc(nodeName)
}

func (cache *FakeCache) DeleteExtendedResource(nodeName string) {
	cache.DeleteCacheFunc(nodeName)
}

func (cache *FakeCache) PrintCacheInfo() {
}
