package main

type interfaceReader interface {
	Read() int
}

type interfaceCloser interface {
	Close() int
}

type interfaceReadCloser interface {
	Read() int
	Close() int
}

type interfaceResource struct {
	base int
}

func (resource *interfaceResource) Read() int {
	return resource.base + 7
}

func (resource *interfaceResource) Close() int {
	return resource.base + 11
}

func main() {
	var combined interfaceReadCloser = &interfaceResource{base: 31}
	var reader interfaceReader = combined
	var closer interfaceCloser = combined

	if reader.Read() != 38 {
		panic("interface-to-interface reader conversion failed")
	}
	if closer.Close() != 42 {
		panic("interface-to-interface closer conversion failed")
	}

	recovered, ok := reader.(interfaceReadCloser)
	if !ok || recovered.Close() != 42 {
		panic("interface assertion back to wider interface failed")
	}
}
